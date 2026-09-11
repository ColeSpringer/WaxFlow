# Encode a WAV to Windows Media Audio with WINDOWS' OWN encoder, through
# Media Foundation's MediaTranscoder, or decode one back with Windows' own
# DECODER (-Decode). It is the second WMA encoder this tree can reach and the
# only one that is not FFmpeg's, which matters more here than it would for any
# other codec: measured, FFmpeg's WMA encoder emits a flat exponent curve,
# never noise-fills a band, always codes both channels, and never sets a flags2
# bit past the first, so a corpus built from it exercises roughly a third of
# the format. Microsoft's sets the bit reservoir, variable block lengths and
# (below 22 kHz) LSP-coded exponents, and noise-fills.
#
# Run under Windows PowerShell 5.1 (powershell.exe), not pwsh: the WinRT type
# projections this needs are 5.1 only. Windows only, by construction.
#
# MediaEncodingProfile.CreateWma produces WMA PRO at every quality level, so
# the profile is built by hand: -Subtype Wma8 is WMA Standard v2 (wFormatTag
# 0x0161) and -Subtype Wma9 is WMA Pro (0x0162). WMA Lossless is neither this
# script's job nor Media Foundation's at all; wmfll.ps1 says why.
#
#   powershell.exe -NoProfile -ExecutionPolicy Bypass -File wmfenc.ps1 `
#       -In src.wav -Out out.wma -Rate 44100 -Channels 2 -BitRate 128000
#
#   powershell.exe -NoProfile -ExecutionPolicy Bypass -File wmfenc.ps1 `
#       -Decode -In out.wma -Out ref.wav
#
# The encoder picks its own bit rate near the request and ignores it entirely
# at low sample rates; the caller reads back what it actually got.

param(
  [Parameter(Mandatory=$true)][string]$In,
  [Parameter(Mandatory=$true)][string]$Out,
  [int]$Rate = 44100,
  [int]$Channels = 2,
  [int]$BitRate = 128000,
  [string]$Subtype = 'Wma8',
  [switch]$Decode,
  [int]$Bits = 16
)

$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Runtime.WindowsRuntime

[Windows.Storage.StorageFile,Windows.Storage,ContentType=WindowsRuntime]                | Out-Null
[Windows.Storage.StorageFolder,Windows.Storage,ContentType=WindowsRuntime]              | Out-Null
[Windows.Media.Transcoding.MediaTranscoder,Windows.Media,ContentType=WindowsRuntime]    | Out-Null
[Windows.Media.MediaProperties.MediaEncodingProfile,Windows.Media,ContentType=WindowsRuntime] | Out-Null
[Windows.Media.MediaProperties.AudioEncodingProperties,Windows.Media,ContentType=WindowsRuntime] | Out-Null
[Windows.Media.MediaProperties.ContainerEncodingProperties,Windows.Media,ContentType=WindowsRuntime] | Out-Null

$ext = [System.WindowsRuntimeSystemExtensions]
$asTaskOp = ($ext.GetMethods() | Where-Object {
  $_.Name -eq 'AsTask' -and $_.IsGenericMethod -and $_.GetParameters().Count -eq 1 -and
  $_.GetParameters()[0].ParameterType.Name -eq 'IAsyncOperation`1' })[0]
$asTaskActProg = ($ext.GetMethods() | Where-Object {
  $_.Name -eq 'AsTask' -and $_.IsGenericMethod -and $_.GetParameters().Count -eq 1 -and
  $_.GetParameters()[0].ParameterType.Name -eq 'IAsyncActionWithProgress`1' })[0]

# Task.Wait throws on a faulted task, so an IsFaulted check after it is dead
# code and the caller sees PowerShell's "Exception calling Wait" wrapper
# instead of the HRESULT underneath. Every failure here is a Media Foundation
# one -- an unsupported rate, a missing codec on this Windows build -- and the
# HRESULT is the whole diagnostic, so it is unwrapped and rethrown.
function Fail($e, $what) {
  while ($e.InnerException) { $e = $e.InnerException }
  throw ("{0}: {1} (0x{2:X8})" -f $what, $e.Message, $e.HResult)
}
function Await($op, $type) {
  $t = $asTaskOp.MakeGenericMethod($type).Invoke($null, @($op))
  try { $t.Wait(-1) | Out-Null } catch { Fail $_.Exception 'await' }
  $t.Result
}
function AwaitActionProgress($act, $ptype) {
  $t = $asTaskActProg.MakeGenericMethod($ptype).Invoke($null, @($act))
  try { $t.Wait(-1) | Out-Null } catch { Fail $_.Exception 'transcode' }
}

# A MediaEncodingSubtypes NAME or a {GUID} string. Both are needed and the
# second is not a convenience: MediaEncodingSubtypes names no constant for WMA
# Voice, so without the GUID form that codec has no encoder in reach at all.
# Reflection rather than ::$Subtype because a mistyped name comes back $null
# from that and fails much later, as a media type with no subtype.
function ResolveSubtype([string]$s) {
  if ($s -match '^\{[0-9A-Fa-f]{8}(-[0-9A-Fa-f]{4}){3}-[0-9A-Fa-f]{12}\}$') { return $s }
  $flags = [Reflection.BindingFlags]'Static,Public,IgnoreCase'
  $p = [Windows.Media.MediaProperties.MediaEncodingSubtypes].GetProperty($s, $flags)
  if (-not $p) {
    throw "-Subtype $s is neither a MediaEncodingSubtypes name nor a {GUID}"
  }
  return $p.GetValue($null)
}

$In  = [System.IO.Path]::GetFullPath($In)
$Out = [System.IO.Path]::GetFullPath($Out)

$src = Await ([Windows.Storage.StorageFile]::GetFileFromPathAsync($In)) ([Windows.Storage.StorageFile])
$dir = Await ([Windows.Storage.StorageFolder]::GetFolderFromPathAsync([System.IO.Path]::GetDirectoryName($Out))) ([Windows.Storage.StorageFolder])

# PrepareFileTranscodeAsync needs the destination file to exist, so the
# destination cannot be created after the CanTranscode check that might refuse.
# It is created under a temporary name instead and moved into place only once
# the transcode has finished: a refusal used to leave the caller's target
# truncated to zero bytes, which reads as a file rather than as a failure.
$tmp = "$Out.wmfenc-tmp"
$dst = Await ($dir.CreateFileAsync([System.IO.Path]::GetFileName($tmp), [Windows.Storage.CreationCollisionOption]::ReplaceExisting)) ([Windows.Storage.StorageFile])

$container = New-Object Windows.Media.MediaProperties.ContainerEncodingProperties
$profile = New-Object Windows.Media.MediaProperties.MediaEncodingProfile

if ($Decode) {
  # Windows' own decoder, as a second oracle beside FFmpeg's. The output shape
  # is the SOURCE's rate and channel count, read off its own profile rather
  # than off -Rate and -Channels: an oracle that resampled would be answering a
  # different question, and a decoder's output shape is one of the things under
  # test. -Bits picks the PCM width, since that one is the caller's choice.
  $sp = Await ([Windows.Media.MediaProperties.MediaEncodingProfile]::CreateFromFileAsync($src)) ([Windows.Media.MediaProperties.MediaEncodingProfile])
  if (-not $sp.Audio) {
    Remove-Item -LiteralPath $tmp -Force -ErrorAction SilentlyContinue
    throw "$In carries no audio stream"
  }
  $what = "decode $($sp.Audio.Subtype) $($sp.Audio.SampleRate)/$($sp.Audio.ChannelCount) to $Bits-bit PCM"
  $audio = [Windows.Media.MediaProperties.AudioEncodingProperties]::CreatePcm(
      $sp.Audio.SampleRate, $sp.Audio.ChannelCount, $Bits)
  $container.Subtype = [Windows.Media.MediaProperties.MediaEncodingSubtypes]::Wave
} else {
  # CreateWma leaves BitsPerSample at 16, and MediaTranscoder then quietly
  # renegotiates to the nearest type it can reach rather than refusing: every
  # WMA Pro format above 48 kHz is 24-bit, so a 96 kHz request at the default
  # depth comes back as a 48 kHz file. -Bits is what makes those reachable.
  $what = "$Subtype $Rate/$Channels/$BitRate/$Bits-bit"
  $audio = [Windows.Media.MediaProperties.AudioEncodingProperties]::CreateWma($Rate, $Channels, $BitRate)
  $audio.BitsPerSample = $Bits
  $audio.Subtype = ResolveSubtype $Subtype
  $container.Subtype = [Windows.Media.MediaProperties.MediaEncodingSubtypes]::Asf
}
$profile.Container = $container
$profile.Audio = $audio

$tr = New-Object Windows.Media.Transcoding.MediaTranscoder
$prep = Await ($tr.PrepareFileTranscodeAsync($src, $dst, $profile)) ([Windows.Media.Transcoding.PrepareTranscodeResult])
if (-not $prep.CanTranscode) {
  Remove-Item -LiteralPath $tmp -Force -ErrorAction SilentlyContinue
  throw "cannot transcode ($what): $($prep.FailureReason)"
}
AwaitActionProgress ($prep.TranscodeAsync()) ([double])
if ((Get-Item -LiteralPath $tmp).Length -eq 0) {
  Remove-Item -LiteralPath $tmp -Force -ErrorAction SilentlyContinue
  throw "the transcode reported success and wrote nothing ($what)"
}
Move-Item -LiteralPath $tmp -Destination $Out -Force
Write-Output "ok $Out"

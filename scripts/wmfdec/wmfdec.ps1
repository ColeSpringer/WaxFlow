# Decode Windows Media Audio to PCM with WINDOWS' OWN decoder, through Media
# Foundation's MediaTranscoder. It is the read-side companion to
# scripts/wmfenc/wmfenc.ps1, and it exists for the same reason: FFmpeg is the
# only other WMA implementation this tree can reach, so a measurement made
# against it alone cannot tell "we decode WMA" from "we agree with FFmpeg".
#
# It is what settled the head-alignment question in docs/notes/
# wma-bitstream.md section 11: Windows' decoder drops one frame from an
# FFmpeg-encoded file and two from a Media Foundation one, and its output is
# otherwise bit-identical to this decoder's. Re-running that measurement needs
# this script.
#
# Run under Windows PowerShell 5.1 (powershell.exe), not pwsh: the WinRT type
# projections this needs are 5.1 only. Windows only, by construction; from WSL
# it is reached over powershell.exe interop with \\wsl.localhost paths.
#
#   powershell.exe -NoProfile -ExecutionPolicy Bypass -File wmfdec.ps1 `
#       -In src.wma -Out out.wav -Rate 44100 -Channels 1 -Bits 16
#
# The rate and channel count are the WAVE profile's, not the source's: Media
# Foundation resamples to whatever is asked for, so a caller measuring
# alignment passes the source's own values and checks what it got back.
#
# It reads WMA-in-WAV (RIFF format tag 0x0161) as happily as ASF, which is how
# a hand-built stream gets a Windows decode without an ASF muxer.

param(
  [Parameter(Mandatory=$true)][string]$In,
  [Parameter(Mandatory=$true)][string]$Out,
  [int]$Rate = 44100,
  [int]$Channels = 1,
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
$In  = [System.IO.Path]::GetFullPath($In)
$Out = [System.IO.Path]::GetFullPath($Out)
$src = Await ([Windows.Storage.StorageFile]::GetFileFromPathAsync($In)) ([Windows.Storage.StorageFile])
$dir = Await ([Windows.Storage.StorageFolder]::GetFolderFromPathAsync([System.IO.Path]::GetDirectoryName($Out))) ([Windows.Storage.StorageFolder])
$tmp = "$Out.wmfdec-tmp"
$dst = Await ($dir.CreateFileAsync([System.IO.Path]::GetFileName($tmp), [Windows.Storage.CreationCollisionOption]::ReplaceExisting)) ([Windows.Storage.StorageFile])
$audio = [Windows.Media.MediaProperties.AudioEncodingProperties]::CreatePcm($Rate, $Channels, $Bits)
$container = New-Object Windows.Media.MediaProperties.ContainerEncodingProperties
$container.Subtype = [Windows.Media.MediaProperties.MediaEncodingSubtypes]::Wave
$profile = New-Object Windows.Media.MediaProperties.MediaEncodingProfile
$profile.Container = $container
$profile.Audio = $audio
$tr = New-Object Windows.Media.Transcoding.MediaTranscoder
$prep = Await ($tr.PrepareFileTranscodeAsync($src, $dst, $profile)) ([Windows.Media.Transcoding.PrepareTranscodeResult])
if (-not $prep.CanTranscode) {
  Remove-Item -LiteralPath $tmp -Force -ErrorAction SilentlyContinue
  throw "cannot transcode: $($prep.FailureReason)"
}
AwaitActionProgress ($prep.TranscodeAsync()) ([double])
Move-Item -LiteralPath $tmp -Destination $Out -Force
Write-Output "ok $Out"

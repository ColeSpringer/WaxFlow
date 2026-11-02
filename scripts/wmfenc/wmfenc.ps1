# Encode a WAV to Windows Media Audio with WINDOWS' OWN encoder, through
# Media Foundation's MediaTranscoder (WMA Voice excepted; the note above
# $VoiceEncoder says why), or decode one back with Windows' own
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

# WMA Voice is not sent through MediaTranscoder. Windows' speech encoder
# (WMSPDMOE.DLL) drops everything it still holds, about 1.8 seconds, when it is
# told to drain before a ProcessOutput call has returned NEED_MORE_INPUT for
# its last input. MediaTranscoder's pipeline sometimes drains it that way, more
# often under load, and the file comes back short with a valid header: every
# short one held exactly the packets made before the drain. Here the encoder
# is driven directly, its output pulled dry after every input and again after
# the drain, and the Sink Writer only writes the ASF file. The WAV must already
# be one of the encoder's seven shapes; a request outside them fails by name
# rather than being renegotiated.
$VoiceSubtype = '{0000000A-0000-0010-8000-00AA00389B71}'
$VoiceEncoder = @'
using System;
using System.Runtime.InteropServices;

// Slots this script never calls are declared bare: only their place in the
// vtable matters.
[ComImport, Guid("2cd2d921-c447-44a7-a13c-4adabfc247e3"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
public interface IMFAttributes {
  void GetItem(); void GetItemType(); void CompareItem(); void Compare();
  [PreserveSig] int GetUINT32(ref Guid key, out int value);
  void GetUINT64(); void GetDouble();
  [PreserveSig] int GetGUID(ref Guid key, out Guid value);
  void GetStringLength(); void GetString(); void GetAllocatedString(); void GetBlobSize();
  void GetBlob(); void GetAllocatedBlob(); void GetUnknown(); void SetItem(); void DeleteItem();
  void DeleteAllItems(); void SetUINT32(); void SetUINT64(); void SetDouble();
  [PreserveSig] int SetGUID(ref Guid key, ref Guid value);
}

[ComImport, Guid("44ae0fa8-ea31-4109-8d2e-4cae4997c555"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
public interface IMFMediaType { }

[ComImport, Guid("045fa593-8799-42b8-bc8d-8968c6453507"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
public interface IMFMediaBuffer { }

[ComImport, Guid("c40a00f2-b93a-4d80-ae8c-5a1c634f58e4"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
public interface IMFSample {
  void GetItem(); void GetItemType(); void CompareItem(); void Compare(); void GetUINT32();
  void GetUINT64(); void GetDouble(); void GetGUID(); void GetStringLength(); void GetString();
  void GetAllocatedString(); void GetBlobSize(); void GetBlob(); void GetAllocatedBlob(); void GetUnknown();
  void SetItem(); void DeleteItem(); void DeleteAllItems(); void SetUINT32(); void SetUINT64();
  void SetDouble(); void SetGUID(); void SetString(); void SetBlob(); void SetUnknown();
  void LockStore(); void UnlockStore(); void GetCount(); void GetItemByIndex(); void CopyAllItems();
  void GetSampleFlags(); void SetSampleFlags(); void GetSampleTime(); void SetSampleTime();
  void GetSampleDuration(); void SetSampleDuration(); void GetBufferCount(); void GetBufferByIndex();
  void ConvertToContiguousBuffer();
  [PreserveSig] int AddBuffer(IMFMediaBuffer buffer);
}

[StructLayout(LayoutKind.Sequential)]
public struct MFT_OUTPUT_STREAM_INFO { public int dwFlags, cbSize, cbAlignment; }

[StructLayout(LayoutKind.Sequential)]
public struct MFT_OUTPUT_DATA_BUFFER { public int dwStreamID; public IntPtr pSample; public int dwStatus; public IntPtr pEvents; }

[ComImport, Guid("bf94c121-5b05-4e6f-8000-ba598961414d"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
public interface IMFTransform {
  void GetStreamLimits(); void GetStreamCount(); void GetStreamIDs(); void GetInputStreamInfo();
  [PreserveSig] int GetOutputStreamInfo(int stream, out MFT_OUTPUT_STREAM_INFO info);
  void GetAttributes(); void GetInputStreamAttributes(); void GetOutputStreamAttributes();
  void DeleteInputStream(); void AddInputStreams();
  [PreserveSig] int GetInputAvailableType(int stream, int index, out IMFMediaType type);
  [PreserveSig] int GetOutputAvailableType(int stream, int index, out IMFMediaType type);
  [PreserveSig] int SetInputType(int stream, IMFMediaType type, int flags);
  [PreserveSig] int SetOutputType(int stream, IMFMediaType type, int flags);
  void GetInputCurrentType(); void GetOutputCurrentType(); void GetInputStatus(); void GetOutputStatus();
  void SetOutputBounds(); void ProcessEvent();
  [PreserveSig] int ProcessMessage(int message, IntPtr param);
  [PreserveSig] int ProcessInput(int stream, IMFSample sample, int flags);
  [PreserveSig] int ProcessOutput(int flags, int count, ref MFT_OUTPUT_DATA_BUFFER buffer, out int status);
}

[ComImport, Guid("70ae66f2-c809-4e4f-8915-bdcb406b7993"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
public interface IMFSourceReader {
  void GetStreamSelection(); void SetStreamSelection(); void GetNativeMediaType();
  [PreserveSig] int GetCurrentMediaType(int stream, out IMFMediaType type);
  void SetCurrentMediaType(); void SetCurrentPosition();
  [PreserveSig] int ReadSample(int stream, int control, out int actual, out int flags, out long time, out IMFSample sample);
}

[ComImport, Guid("3137f1cd-fe5e-4805-a5d8-fb477448cb3d"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
public interface IMFSinkWriter {
  [PreserveSig] int AddStream(IMFMediaType type, out int stream);
  [PreserveSig] int SetInputMediaType(int stream, IMFMediaType type, IntPtr parameters);
  [PreserveSig] int BeginWriting();
  [PreserveSig] int WriteSample(int stream, IMFSample sample);
  void SendStreamTick(); void PlaceMarker(); void NotifyEndOfSegment(); void Flush();
  [PreserveSig] int Complete(); // IMFSinkWriter::Finalize
}

public static class WmfVoice {
  [DllImport("mfplat.dll")] static extern int MFStartup(int version, int flags);
  [DllImport("mfplat.dll")] static extern int MFShutdown();
  [DllImport("mfplat.dll")] static extern int MFCreateAttributes(out IMFAttributes attrs, int size);
  [DllImport("mfplat.dll")] static extern int MFCreateSample(out IMFSample sample);
  [DllImport("mfplat.dll")] static extern int MFCreateMemoryBuffer(int size, out IMFMediaBuffer buffer);
  [DllImport("mfreadwrite.dll")] static extern int MFCreateSourceReaderFromURL(
      [MarshalAs(UnmanagedType.LPWStr)] string url, IntPtr attrs, out IMFSourceReader reader);
  [DllImport("mfreadwrite.dll")] static extern int MFCreateSinkWriterFromURL(
      [MarshalAs(UnmanagedType.LPWStr)] string url, IntPtr stream, IMFAttributes attrs, out IMFSinkWriter writer);

  // WM Speech Encoder DMO, which Media Foundation registers as an MFT.
  static readonly Guid SpeechEncoder = new Guid("1f1f4e1a-2252-4063-84bb-eee75f8856d5");
  static Guid Subtype = new Guid("f7e34c9a-42e8-4714-b74b-cb29d72c35e5");
  static Guid Channels = new Guid("37e48bf5-645e-4c5b-89de-ada9e29b696a");
  static Guid Rate = new Guid("5faeeae7-0290-4c31-9e8a-c534f68d9dba");
  static Guid ByteRate = new Guid("1aab75c8-cfef-451c-ab95-ac034b8e1731");
  static Guid Bits = new Guid("f2deb57f-40fa-4764-aa33-ed4f2d1ff669");
  static Guid ContainerType = new Guid("150ff23f-4abc-478b-ac4f-e1916fba1cca");
  static Guid Asf = new Guid("430f6f6e-b6bf-4fc1-a0bd-9ee46eee2afb");

  const int FirstAudio = unchecked((int)0xFFFFFFFD), ReaderError = 1, ReaderEnd = 2;
  const int Drain = 1, BeginStreaming = 0x10000000, EndOfStream = 0x10000002, StartOfStream = 0x10000003;
  const int NeedMoreInput = unchecked((int)0xC00D6D72), NoMoreTypes = unchecked((int)0xC00D36B9);

  static void Check(int hr, string what) {
    if (hr < 0) throw new COMException(what + " failed", hr);
  }

  static int U32(IMFMediaType t, Guid key) {
    int v = 0;
    ((IMFAttributes)t).GetUINT32(ref key, out v);
    return v;
  }

  static string Shape(IMFMediaType t) {
    return string.Format("{0} Hz {1}ch {2}-bit {3} bit/s", U32(t, Rate), U32(t, Channels), U32(t, Bits), U32(t, ByteRate) * 8);
  }

  public static void Encode(string wav, string output, int rate, int channels, int bitRate, int bits) {
    Check(MFStartup(0x20070, 0), "MFStartup");
    IMFSourceReader reader = null;
    IMFSinkWriter writer = null;
    IMFTransform enc = null;
    IMFMediaType wavType = null, outType = null, inType = null;
    IMFAttributes attrs = null;
    try {
      Check(MFCreateSourceReaderFromURL(wav, IntPtr.Zero, out reader), "opening " + wav);
      Check(reader.GetCurrentMediaType(FirstAudio, out wavType), "reading the WAV's format");

      enc = (IMFTransform)Activator.CreateInstance(Type.GetTypeFromCLSID(SpeechEncoder));
      var offered = new System.Collections.Generic.List<string>();
      for (int i = 0; ; i++) {
        IMFMediaType t;
        int hr = enc.GetOutputAvailableType(0, i, out t);
        if (hr == NoMoreTypes) break;
        Check(hr, "GetOutputAvailableType");
        if (U32(t, Rate) == rate && U32(t, Channels) == channels && U32(t, ByteRate) * 8 == bitRate && U32(t, Bits) == bits) {
          outType = t;
          break;
        }
        offered.Add(Shape(t));
        Marshal.ReleaseComObject(t);
      }
      if (outType == null && offered.Count == 0) throw new Exception("the speech encoder offers no output formats at all");
      if (outType == null) {
        throw new Exception(string.Format("the speech encoder offers no {0} Hz {1}ch {2}-bit {3} bit/s format; it offers {4}",
            rate, channels, bits, bitRate, string.Join("; ", offered)));
      }
      Check(enc.SetOutputType(0, outType, 0), "SetOutputType");
      Check(enc.GetInputAvailableType(0, 0, out inType), "GetInputAvailableType");
      Guid pcm = new Guid("00000001-0000-0010-8000-00aa00389b71"), sub;
      ((IMFAttributes)wavType).GetGUID(ref Subtype, out sub);
      if (sub != pcm || U32(wavType, Rate) != U32(inType, Rate) || U32(wavType, Channels) != U32(inType, Channels) ||
          U32(wavType, Bits) != U32(inType, Bits)) {
        throw new Exception(string.Format("the WAV is {0} Hz {1}ch {2}-bit; this format encodes from {3} Hz {4}ch {5}-bit PCM",
            U32(wavType, Rate), U32(wavType, Channels), U32(wavType, Bits), U32(inType, Rate), U32(inType, Channels), U32(inType, Bits)));
      }
      Check(enc.SetInputType(0, inType, 0), "SetInputType");
      MFT_OUTPUT_STREAM_INFO info;
      Check(enc.GetOutputStreamInfo(0, out info), "GetOutputStreamInfo");

      Check(MFCreateAttributes(out attrs, 1), "MFCreateAttributes");
      Check(attrs.SetGUID(ref ContainerType, ref Asf), "SetGUID");
      Check(MFCreateSinkWriterFromURL(output, IntPtr.Zero, attrs, out writer), "creating " + output);
      int stream;
      Check(writer.AddStream(outType, out stream), "AddStream");
      Check(writer.SetInputMediaType(stream, outType, IntPtr.Zero), "SetInputMediaType");
      Check(writer.BeginWriting(), "BeginWriting");

      Check(enc.ProcessMessage(BeginStreaming, IntPtr.Zero), "BEGIN_STREAMING");
      Check(enc.ProcessMessage(StartOfStream, IntPtr.Zero), "START_OF_STREAM");
      while (true) {
        int actual, flags;
        long time;
        IMFSample s;
        Check(reader.ReadSample(FirstAudio, 0, out actual, out flags, out time, out s), "ReadSample");
        if ((flags & ReaderError) != 0) throw new Exception("the source reader failed partway through " + wav);
        if (s != null) {
          try {
            Check(enc.ProcessInput(0, s, 0), "ProcessInput");
          } finally {
            Marshal.ReleaseComObject(s);
          }
          Pull(enc, writer, stream, info.cbSize);
        }
        if ((flags & ReaderEnd) != 0) break;
      }
      Check(enc.ProcessMessage(EndOfStream, IntPtr.Zero), "END_OF_STREAM");
      Check(enc.ProcessMessage(Drain, IntPtr.Zero), "DRAIN");
      Pull(enc, writer, stream, info.cbSize);
      Check(writer.Complete(), "Finalize");
    } finally {
      // Everything is released before MFShutdown, the writer first: it holds
      // the output open, and the caller deletes it after a failure.
      foreach (object o in new object[] { writer, reader, enc, outType, inType, wavType, attrs }) {
        if (o != null) Marshal.ReleaseComObject(o);
      }
      MFShutdown();
    }
  }

  // Pull takes every output the encoder has until it asks for more input,
  // which is the state it must be in when the drain arrives.
  static void Pull(IMFTransform enc, IMFSinkWriter writer, int stream, int size) {
    while (true) {
      IMFSample s;
      Check(MFCreateSample(out s), "MFCreateSample");
      IMFMediaBuffer b = null;
      try {
        Check(MFCreateMemoryBuffer(size, out b), "MFCreateMemoryBuffer");
        Check(s.AddBuffer(b), "AddBuffer");
        IntPtr p = Marshal.GetComInterfaceForObject(s, typeof(IMFSample));
        var o = new MFT_OUTPUT_DATA_BUFFER();
        o.pSample = p;
        int status;
        int hr = enc.ProcessOutput(0, 1, ref o, out status);
        Marshal.Release(p);
        if (o.pEvents != IntPtr.Zero) Marshal.Release(o.pEvents);
        if (hr == NeedMoreInput) return;
        Check(hr, "ProcessOutput");
        Check(writer.WriteSample(stream, s), "WriteSample");
      } finally {
        if (b != null) Marshal.ReleaseComObject(b);
        Marshal.ReleaseComObject(s);
      }
    }
  }
}
'@

$In  = [System.IO.Path]::GetFullPath($In)
$Out = [System.IO.Path]::GetFullPath($Out)

# The output is built under a temporary name and moved into place only once it
# is complete: a refusal used to leave the caller's target truncated to zero
# bytes, which reads as a file rather than as a failure.
$tmp = "$Out.wmfenc-tmp"

if (-not $Decode -and $Subtype -eq $VoiceSubtype) {
  $what = "WMA Voice $Rate/$Channels/$BitRate/$Bits-bit"
  Add-Type -TypeDefinition $VoiceEncoder
  try {
    [WmfVoice]::Encode($In, $tmp, $Rate, $Channels, $BitRate, $Bits)
  } catch {
    Remove-Item -LiteralPath $tmp -Force -ErrorAction SilentlyContinue
    Fail $_.Exception $what
  }
} else {
  $src = Await ([Windows.Storage.StorageFile]::GetFileFromPathAsync($In)) ([Windows.Storage.StorageFile])
  $dir = Await ([Windows.Storage.StorageFolder]::GetFolderFromPathAsync([System.IO.Path]::GetDirectoryName($Out))) ([Windows.Storage.StorageFolder])

  # PrepareFileTranscodeAsync needs the destination file to exist, so it is
  # created before the CanTranscode check that might refuse.
  $dst = Await ($dir.CreateFileAsync([System.IO.Path]::GetFileName($tmp), [Windows.Storage.CreationCollisionOption]::ReplaceExisting)) ([Windows.Storage.StorageFile])

  $container = New-Object Windows.Media.MediaProperties.ContainerEncodingProperties
  $profile = New-Object Windows.Media.MediaProperties.MediaEncodingProfile

  if ($Decode) {
    # Windows' own decoder, as a second oracle beside FFmpeg's. The output
    # shape is the SOURCE's rate and channel count, read off its own profile
    # rather than off -Rate and -Channels: an oracle that resampled would be
    # answering a different question, and a decoder's output shape is one of
    # the things under test. -Bits picks the PCM width, since that one is the
    # caller's choice.
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
    # renegotiates to the nearest type it can reach rather than refusing:
    # every WMA Pro format above 48 kHz is 24-bit, so a 96 kHz request at the
    # default depth comes back as a 48 kHz file. -Bits is what makes those
    # reachable.
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
}
if ((Get-Item -LiteralPath $tmp).Length -eq 0) {
  Remove-Item -LiteralPath $tmp -Force -ErrorAction SilentlyContinue
  throw "the transcode reported success and wrote nothing ($what)"
}
Move-Item -LiteralPath $tmp -Destination $Out -Force
Write-Output "ok $Out"

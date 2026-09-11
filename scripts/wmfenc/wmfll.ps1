# Encode a WAV to WMA LOSSLESS (wFormatTag 0x0163) with Windows' own encoder.
#
# There is no other encoder for this format. FFmpeg decodes WMA Lossless and
# cannot write it, so without this script the codec has no fixtures at all and
# nothing to be bit-exact against.
#
# It does NOT go through MediaTranscoder the way wmfenc.ps1 does, and the two
# reasons are worth writing down because both look like bugs from the outside:
#
#  1. MediaTranscoder refuses the lossless subtype with InvalidProfile at every
#     bit rate, and Media Foundation's ASF file sink refuses the stream type
#     outright (MF_E_ASF_UNSUPPORTED_STREAM_TYPE) even when handed a media type
#     the encoder itself produced. The Media Foundation write path cannot
#     produce this format on Windows 11.
#  2. MFTranscodeGetAudioOutputAvailableTypes returns ZERO output types for the
#     lossless subtype, although the WMAudio Encoder MFT registers 0x0163 among
#     its outputs, and the encoder MFT offers none after SetInputType either. A
#     lossless output type has no bit rate to enumerate over: its output IS its
#     input, so it exists only relative to one.
#
# What works is the Windows Media Format SDK (wmvcore), which is what Windows
# Media Player's own lossless rip uses. The one non-obvious step is that
# IWMCodecInfo3 lists the "Windows Media Audio 9.2 Lossless" codec with ZERO
# formats until the enumeration setting `_VBRENABLED` is set on it; with it set
# the codec enumerates eight, each carrying the 18 bytes of codec private data
# the encoder will accept. Those eight are the whole envelope:
#
#     44100 Hz  stereo  16-bit
#     44100 Hz  stereo  24-bit
#     48000 Hz  stereo / 5.1  24-bit
#     88200 Hz  stereo / 5.1  24-bit
#     96000 Hz  stereo / 5.1  24-bit
#
# There is no mono format, no 16-bit format above 44.1 kHz, and no 5.1 format
# at 44.1 kHz. Run with -List to print what this Windows build actually offers
# rather than trusting the list above.
#
# The format is chosen to match the input WAV exactly, which is what makes the
# source PCM the oracle: a lossless decode must return the encoder's input
# sample for sample.
#
# Run under Windows PowerShell 5.1 (powershell.exe), not pwsh. Windows only, by
# construction; from WSL it is reached over powershell.exe interop with
# \\wsl.localhost paths.
#
#   powershell.exe -NoProfile -ExecutionPolicy Bypass -File wmfll.ps1 `
#       -In src.wav -Out out.wma
#   powershell.exe -NoProfile -ExecutionPolicy Bypass -File wmfll.ps1 -List

param(
  [string]$In,
  [string]$Out,
  [switch]$List
)

$ErrorActionPreference = 'Stop'

Add-Type -Language CSharp @'
using System;
using System.IO;
using System.Collections.Generic;
using System.Runtime.InteropServices;

[ComImport, Guid("d16679f2-6ca0-472d-8d31-2f5d55aee155"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
public interface IWMProfileManager {
  [PreserveSig] int CreateEmptyProfile(uint ver, out IntPtr p);
  [PreserveSig] int LoadProfileByID(ref Guid g, out IntPtr p);
  [PreserveSig] int LoadProfileByData([MarshalAs(UnmanagedType.LPWStr)] string s, out IntPtr p);
  [PreserveSig] int SaveProfile(IntPtr p, IntPtr s, ref uint n);
  [PreserveSig] int GetSystemProfileCount(out uint n);
  [PreserveSig] int LoadSystemProfile(uint i, out IntPtr p);
}

[ComImport, Guid("7e51f487-4d93-4f98-8ab4-27d0565adc51"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
public interface IWMCodecInfo3 {
  [PreserveSig] int GetCodecInfoCount(ref Guid type, out uint n);
  [PreserveSig] int GetCodecFormatCount(ref Guid type, uint codec, out uint n);
  [PreserveSig] int GetCodecFormat(ref Guid type, uint codec, uint fmt, out IWMStreamConfig cfg);
  [PreserveSig] int GetCodecName(ref Guid type, uint codec, IntPtr name, ref uint cch);
  [PreserveSig] int GetCodecFormatDesc(ref Guid type, uint codec, uint fmt, out IWMStreamConfig cfg, IntPtr desc, ref uint cch);
  [PreserveSig] int GetCodecFormatProp(ref Guid type, uint codec, uint fmt, [MarshalAs(UnmanagedType.LPWStr)] string name, out uint dt, IntPtr val, ref uint len);
  [PreserveSig] int GetCodecProp(ref Guid type, uint codec, [MarshalAs(UnmanagedType.LPWStr)] string name, out uint dt, IntPtr val, ref uint len);
  [PreserveSig] int SetCodecEnumerationSetting(ref Guid type, uint codec, [MarshalAs(UnmanagedType.LPWStr)] string name, uint dt, [In, MarshalAs(UnmanagedType.LPArray)] byte[] val, uint len);
  [PreserveSig] int GetCodecEnumerationSetting(ref Guid type, uint codec, [MarshalAs(UnmanagedType.LPWStr)] string name, out uint dt, IntPtr val, ref uint len);
}

[ComImport, Guid("96406bdc-2b2b-11d3-b36b-00c04f6108ff"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
public interface IWMStreamConfig {
  [PreserveSig] int GetStreamType(out Guid g);
  [PreserveSig] int GetStreamNumber(out ushort n);
  [PreserveSig] int SetStreamNumber(ushort n);
  [PreserveSig] int GetStreamName(IntPtr s, ref ushort n);
  [PreserveSig] int SetStreamName([MarshalAs(UnmanagedType.LPWStr)] string s);
  [PreserveSig] int GetConnectionName(IntPtr s, ref ushort n);
  [PreserveSig] int SetConnectionName([MarshalAs(UnmanagedType.LPWStr)] string s);
  [PreserveSig] int GetBitrate(out uint b);
  [PreserveSig] int SetBitrate(uint b);
  [PreserveSig] int GetBufferWindow(out uint w);
  [PreserveSig] int SetBufferWindow(uint w);
}

[ComImport, Guid("96406bce-2b2b-11d3-b36b-00c04f6108ff"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
public interface IWMMediaProps {
  [PreserveSig] int GetType(out Guid g);
  [PreserveSig] int GetMediaType(IntPtr mt, ref uint cb);
  [PreserveSig] int SetMediaType(IntPtr mt);
}

[ComImport, Guid("96406bdb-2b2b-11d3-b36b-00c04f6108ff"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
public interface IWMProfile {
  [PreserveSig] int GetVersion(out uint v);
  [PreserveSig] int GetName(IntPtr s, ref uint n);
  [PreserveSig] int SetName([MarshalAs(UnmanagedType.LPWStr)] string s);
  [PreserveSig] int GetDescription(IntPtr s, ref uint n);
  [PreserveSig] int SetDescription([MarshalAs(UnmanagedType.LPWStr)] string s);
  [PreserveSig] int GetStreamCount(out uint n);
  [PreserveSig] int GetStream(uint i, out IWMStreamConfig c);
  [PreserveSig] int GetStreamByNumber(ushort n, out IWMStreamConfig c);
  [PreserveSig] int RemoveStream(IWMStreamConfig c);
  [PreserveSig] int RemoveStreamByNumber(ushort n);
  [PreserveSig] int AddStream(IWMStreamConfig c);
  [PreserveSig] int ReconfigStream(IWMStreamConfig c);
  [PreserveSig] int CreateNewStream(ref Guid type, out IWMStreamConfig c);
  [PreserveSig] int GetMutualExclusionCount(out uint n);
  [PreserveSig] int GetMutualExclusion(uint i, out IntPtr m);
  [PreserveSig] int RemoveMutualExclusion(IntPtr m);
  [PreserveSig] int AddMutualExclusion(IntPtr m);
  [PreserveSig] int CreateNewMutualExclusion(out IntPtr m);
}

[ComImport, Guid("96406bd5-2b2b-11d3-b36b-00c04f6108ff"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
public interface IWMInputMediaProps {
  [PreserveSig] int GetType(out Guid g);
  [PreserveSig] int GetMediaType(IntPtr mt, ref uint cb);
  [PreserveSig] int SetMediaType(IntPtr mt);
  [PreserveSig] int GetConnectionName(IntPtr s, ref ushort n);
  [PreserveSig] int GetGroupName(IntPtr s, ref ushort n);
}

[ComImport, Guid("e1cd3524-03d7-11d2-9eed-006097d2d7cf"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
public interface INSSBuffer {
  [PreserveSig] int GetLength(out uint n);
  [PreserveSig] int SetLength(uint n);
  [PreserveSig] int GetMaxLength(out uint n);
  [PreserveSig] int GetBuffer(out IntPtr p);
  [PreserveSig] int GetBufferAndLength(out IntPtr p, out uint n);
}

[ComImport, Guid("96406bd4-2b2b-11d3-b36b-00c04f6108ff"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
public interface IWMWriter {
  [PreserveSig] int SetProfileByID(ref Guid g);
  [PreserveSig] int SetProfile(IWMProfile p);
  [PreserveSig] int SetOutputFilename([MarshalAs(UnmanagedType.LPWStr)] string s);
  [PreserveSig] int GetInputCount(out uint n);
  [PreserveSig] int GetInputProps(uint i, out IWMInputMediaProps p);
  [PreserveSig] int SetInputProps(uint i, IWMInputMediaProps p);
  [PreserveSig] int GetInputFormatCount(uint i, out uint n);
  [PreserveSig] int GetInputFormat(uint i, uint j, out IWMInputMediaProps p);
  [PreserveSig] int BeginWriting();
  [PreserveSig] int EndWriting();
  [PreserveSig] int AllocateSample(uint cb, out INSSBuffer b);
  [PreserveSig] int WriteSample(uint input, ulong time, uint flags, INSSBuffer b);
  [PreserveSig] int Flush();
}

public static class WmfLossless {
  [DllImport("wmvcore.dll")] static extern int WMCreateProfileManager(out IWMProfileManager pm);
  [DllImport("wmvcore.dll")] static extern int WMCreateWriter(IntPtr cert, out IWMWriter w);

  static Guid AudioType = new Guid("73647561-0000-0010-8000-00aa00389b71");
  static Guid PcmSub    = new Guid("00000001-0000-0010-8000-00aa00389b71");
  static Guid FmtWaveEx = new Guid("05589f81-c356-11ce-bf01-00aa0055595a");

  // WM_MEDIA_TYPE on x64: majortype 0, subtype 16, bFixedSizeSamples 32,
  // bTemporalCompression 36, lSampleSize 40, formattype 44, pUnk 64 (the
  // pointer realigns), cbFormat 72, pbFormat 80.
  const int OFF_CBFORMAT = 72, OFF_PBFORMAT = 80, SIZEOF_WMT = 88;
  const uint WMT_VER_9_0 = 0x00090000, WMT_TYPE_BOOL = 3, WM_SF_CLEANPOINT = 1;

  public class Wav { public int Rate, Channels, Bits; public byte[] Data; }

  // A canonical PCM WAV, walked for its fmt and data chunks. WAVE_FORMAT_
  // EXTENSIBLE carries the real depth in the same two fields, so both spell
  // the shape this needs.
  public static Wav ReadWav(string path) {
    byte[] b = File.ReadAllBytes(path);
    var w = new Wav();
    int p = 12;
    while (p + 8 <= b.Length) {
      string id = System.Text.Encoding.ASCII.GetString(b, p, 4);
      int n = BitConverter.ToInt32(b, p + 4);
      if (id == "fmt ") {
        w.Channels = BitConverter.ToInt16(b, p + 10);
        w.Rate = BitConverter.ToInt32(b, p + 12);
        w.Bits = BitConverter.ToInt16(b, p + 22);
      } else if (id == "data") {
        w.Data = new byte[n];
        Array.Copy(b, p + 8, w.Data, 0, n);
        break;
      }
      p += 8 + n + (n & 1);
    }
    if (w.Data == null) throw new Exception("no data chunk in " + path);
    return w;
  }

  static IWMCodecInfo3 CodecInfo() {
    IWMProfileManager pm;
    int hr = WMCreateProfileManager(out pm);
    if (hr != 0) throw new Exception(string.Format("WMCreateProfileManager 0x{0:X8}", hr));
    return (IWMCodecInfo3)pm;
  }

  static string CodecName(IWMCodecInfo3 ci, uint i) {
    uint cch = 0;
    ci.GetCodecName(ref AudioType, i, IntPtr.Zero, ref cch);
    IntPtr nm = Marshal.AllocHGlobal((int)cch * 2 + 2);
    try {
      ci.GetCodecName(ref AudioType, i, nm, ref cch);
      return Marshal.PtrToStringUni(nm);
    } finally { Marshal.FreeHGlobal(nm); }
  }

  // The lossless codec's index, with its format enumeration unlocked. Without
  // _VBRENABLED the codec is listed and offers nothing.
  static uint LosslessCodec(IWMCodecInfo3 ci) {
    uint n;
    int hr = ci.GetCodecInfoCount(ref AudioType, out n);
    if (hr != 0) throw new Exception(string.Format("GetCodecInfoCount 0x{0:X8}", hr));
    for (uint i = 0; i < n; i++) {
      if (CodecName(ci, i).IndexOf("Lossless") < 0) continue;
      hr = ci.SetCodecEnumerationSetting(ref AudioType, i, "_VBRENABLED", WMT_TYPE_BOOL, BitConverter.GetBytes(1), 4);
      if (hr != 0) throw new Exception(string.Format("SetCodecEnumerationSetting(_VBRENABLED) 0x{0:X8}", hr));
      return i;
    }
    throw new Exception("this Windows build has no WMA Lossless encoder");
  }

  static byte[] WaveFormatOf(IWMStreamConfig cfg) {
    IWMMediaProps mp = (IWMMediaProps)cfg;
    uint cb = 0;
    mp.GetMediaType(IntPtr.Zero, ref cb);
    IntPtr mt = Marshal.AllocHGlobal((int)cb);
    try {
      mp.GetMediaType(mt, ref cb);
      uint cbFmt = (uint)Marshal.ReadInt32(mt, OFF_CBFORMAT);
      IntPtr pb = Marshal.ReadIntPtr(mt, OFF_PBFORMAT);
      byte[] wf = new byte[cbFmt];
      Marshal.Copy(pb, wf, 0, (int)cbFmt);
      return wf;
    } finally { Marshal.FreeHGlobal(mt); }
  }

  public static string[] Formats() {
    var lines = new List<string>();
    IWMCodecInfo3 ci = CodecInfo();
    uint codec = LosslessCodec(ci);
    uint fc;
    ci.GetCodecFormatCount(ref AudioType, codec, out fc);
    for (uint j = 0; j < fc; j++) {
      IWMStreamConfig cfg;
      if (ci.GetCodecFormat(ref AudioType, codec, j, out cfg) != 0) continue;
      byte[] wf = WaveFormatOf(cfg);
      string ex = "";
      for (int k = 18; k < wf.Length; k++) ex += wf[k].ToString("x2");
      lines.Add(string.Format("[{0}] tag=0x{1:x4} rate={2} ch={3} bits={4} blockAlign={5} cbSize={6} extra={7}",
        j, BitConverter.ToUInt16(wf, 0), BitConverter.ToUInt32(wf, 4), BitConverter.ToUInt16(wf, 2),
        BitConverter.ToUInt16(wf, 14), BitConverter.ToUInt16(wf, 12), BitConverter.ToUInt16(wf, 16), ex));
      Marshal.ReleaseComObject(cfg);
    }
    return lines.ToArray();
  }

  static IWMStreamConfig MatchingFormat(IWMCodecInfo3 ci, uint codec, Wav w) {
    uint fc;
    ci.GetCodecFormatCount(ref AudioType, codec, out fc);
    var offered = "";
    for (uint j = 0; j < fc; j++) {
      IWMStreamConfig cfg;
      if (ci.GetCodecFormat(ref AudioType, codec, j, out cfg) != 0) continue;
      byte[] wf = WaveFormatOf(cfg);
      int rate = (int)BitConverter.ToUInt32(wf, 4), ch = BitConverter.ToUInt16(wf, 2), bits = BitConverter.ToUInt16(wf, 14);
      if (rate == w.Rate && ch == w.Channels && bits == w.Bits) return cfg;
      offered += string.Format(" {0}/{1}ch/{2}bit", rate, ch, bits);
      Marshal.ReleaseComObject(cfg);
    }
    throw new Exception(string.Format(
      "the lossless encoder offers no {0} Hz {1}ch {2}-bit format; it offers{3}",
      w.Rate, w.Channels, w.Bits, offered));
  }

  // The writer's input type. Plain WAVEFORMATEX for one or two channels;
  // WAVEFORMATEXTENSIBLE above that, because a bare WAVEFORMATEX names no
  // channel positions and the writer needs them to match the stream's mask.
  static IntPtr PcmMediaType(Wav w, uint mask) {
    int cbFmt = w.Channels > 2 ? 40 : 18;
    IntPtr mt = Marshal.AllocHGlobal(SIZEOF_WMT + cbFmt);
    for (int i = 0; i < SIZEOF_WMT + cbFmt; i++) Marshal.WriteByte(mt, i, 0);
    IntPtr pb = new IntPtr(mt.ToInt64() + SIZEOF_WMT);
    ushort align = (ushort)(w.Channels * w.Bits / 8);
    Marshal.Copy(AudioType.ToByteArray(), 0, mt, 16);
    Marshal.Copy(PcmSub.ToByteArray(), 0, new IntPtr(mt.ToInt64() + 16), 16);
    Marshal.WriteInt32(mt, 32, 1);        // bFixedSizeSamples
    Marshal.WriteInt32(mt, 40, align);    // lSampleSize
    Marshal.Copy(FmtWaveEx.ToByteArray(), 0, new IntPtr(mt.ToInt64() + 44), 16);
    Marshal.WriteInt32(mt, OFF_CBFORMAT, cbFmt);
    Marshal.WriteIntPtr(mt, OFF_PBFORMAT, pb);

    byte[] wfx = new byte[cbFmt];
    BitConverter.GetBytes((ushort)(cbFmt == 18 ? 1 : 0xFFFE)).CopyTo(wfx, 0);
    BitConverter.GetBytes((ushort)w.Channels).CopyTo(wfx, 2);
    BitConverter.GetBytes((uint)w.Rate).CopyTo(wfx, 4);
    BitConverter.GetBytes((uint)(align * w.Rate)).CopyTo(wfx, 8);
    BitConverter.GetBytes(align).CopyTo(wfx, 12);
    BitConverter.GetBytes((ushort)w.Bits).CopyTo(wfx, 14);
    BitConverter.GetBytes((ushort)(cbFmt == 18 ? 0 : 22)).CopyTo(wfx, 16);
    if (cbFmt > 18) {
      BitConverter.GetBytes((ushort)w.Bits).CopyTo(wfx, 18);   // wValidBitsPerSample
      BitConverter.GetBytes(mask).CopyTo(wfx, 20);             // dwChannelMask
      PcmSub.ToByteArray().CopyTo(wfx, 24);                    // SubFormat
    }
    Marshal.Copy(wfx, 0, pb, cbFmt);
    return mt;
  }

  public static void Encode(string wav, string outPath) {
    Wav w = ReadWav(wav);
    IWMCodecInfo3 ci = CodecInfo();
    uint codec = LosslessCodec(ci);
    IWMStreamConfig cfg = MatchingFormat(ci, codec, w);
    uint mask = BitConverter.ToUInt32(WaveFormatOf(cfg), 20);

    Check(cfg.SetStreamNumber(1), "SetStreamNumber");
    IntPtr profPtr;
    Check(((IWMProfileManager)ci).CreateEmptyProfile(WMT_VER_9_0, out profPtr), "CreateEmptyProfile");
    IWMProfile prof = (IWMProfile)Marshal.GetObjectForIUnknown(profPtr);
    Check(prof.AddStream(cfg), "AddStream");

    IWMWriter wr;
    Check(WMCreateWriter(IntPtr.Zero, out wr), "WMCreateWriter");
    // Released in the finally rather than on the way out: the writer holds the
    // output file open, and a failure partway through would otherwise leave
    // the caller unable to delete the half-written temporary it is about to
    // try to delete.
    try {
      Check(wr.SetProfile(prof), "SetProfile");
      Check(wr.SetOutputFilename(outPath), "SetOutputFilename");

      IWMInputMediaProps ip;
      Check(wr.GetInputProps(0, out ip), "GetInputProps");
      IntPtr mt = PcmMediaType(w, mask);
      try {
        Check(ip.SetMediaType(mt), "SetMediaType");
        Check(wr.SetInputProps(0, ip), "SetInputProps");
      } finally { Marshal.FreeHGlobal(mt); }

      Check(wr.BeginWriting(), "BeginWriting");
      int align = w.Channels * w.Bits / 8;
      int frames = w.Data.Length / align, chunk = 4096;
      for (int f = 0; f < frames; f += chunk) {
        int cnt = Math.Min(chunk, frames - f);
        uint cb = (uint)(cnt * align);
        INSSBuffer buf;
        Check(wr.AllocateSample(cb, out buf), "AllocateSample");
        IntPtr p;
        Check(buf.GetBuffer(out p), "GetBuffer");
        Marshal.Copy(w.Data, f * align, p, (int)cb);
        Check(buf.SetLength(cb), "SetLength");
        Check(wr.WriteSample(0, (ulong)((long)f * 10000000L / w.Rate), WM_SF_CLEANPOINT, buf), "WriteSample");
        Marshal.ReleaseComObject(buf);
      }
      Check(wr.EndWriting(), "EndWriting");
    } finally {
      Marshal.ReleaseComObject(wr);
      Marshal.ReleaseComObject(prof);
      Marshal.ReleaseComObject(cfg);
    }
  }

  static void Check(int hr, string what) {
    if (hr != 0) throw new Exception(string.Format("{0} 0x{1:X8}", what, hr));
  }
}
'@

if ($List) {
  foreach ($l in [WmfLossless]::Formats()) { Write-Output $l }
  exit 0
}
if (-not $In -or -not $Out) { throw "-In and -Out are required unless -List is given" }

$In  = [System.IO.Path]::GetFullPath($In)
$Out = [System.IO.Path]::GetFullPath($Out)

# The writer creates its output up front, so a refusal partway through would
# otherwise leave the caller's target as a short file that reads as a fixture.
# It is built under a temporary name and moved into place only on success.
$tmp = "$Out.wmfll-tmp"
try {
  [WmfLossless]::Encode($In, $tmp)
} catch {
  Remove-Item -LiteralPath $tmp -Force -ErrorAction SilentlyContinue
  $e = $_.Exception
  while ($e.InnerException) { $e = $e.InnerException }
  throw ("wmfll: " + $e.Message)
}
if ((Get-Item -LiteralPath $tmp).Length -eq 0) {
  Remove-Item -LiteralPath $tmp -Force -ErrorAction SilentlyContinue
  throw "wmfll: the encoder reported success and wrote nothing"
}
Move-Item -LiteralPath $tmp -Destination $Out -Force
Write-Output "ok $Out"

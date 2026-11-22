# 截取屏幕（整屏 / 指定窗口客户区 / 指定矩形）并做 OCR，输出一行 JSON。
# 用法:
#   powershell -NoProfile -ExecutionPolicy Bypass -File ocr.ps1 [-Window "标题片段"] [-X 0 -Y 0 -W 0 -H 0] [-Lang en-GB]
#
# 优先级：-W/-H > 0 时用显式矩形；否则 -Window 非空时按窗口标题找客户区；都不可用则整屏。
# 输出: {"ok":true,"text":"...","w":1920,"h":1080,"ms":812,"source":"window","window":"CONTACT"}
# 找不到窗口时回退整屏并带 "fallback":"window-not-found"。
#
# 注意：注释行末尾不能出现反引号，PowerShell 会把它当续行，把下一行也吞进注释。
param(
  [int]$X = 0,
  [int]$Y = 0,
  [int]$W = 0,
  [int]$H = 0,
  [string]$Lang = 'en-GB',
  [string]$Window = '',
  [string]$WindowClass = '',
  [string]$Jpeg = ''
)

$ErrorActionPreference = 'Stop'
$sw = [System.Diagnostics.Stopwatch]::StartNew()
$png = Join-Path $env:TEMP ("dsh-st-ocr-{0}.png" -f [guid]::NewGuid().ToString('N'))
$stream = $null
try {
  Add-Type -AssemblyName System.Drawing
  Add-Type -AssemblyName System.Windows.Forms
  Add-Type -AssemblyName System.Runtime.WindowsRuntime

  # 声明 DPI 感知：不声明时拿到的是缩放后的逻辑像素，OCR 分辨率会被白白砍掉
  try {
    Add-Type -TypeDefinition @'
using System;
using System.Text;
using System.Collections.Generic;
using System.Runtime.InteropServices;
public static class WinOcr {
  [DllImport("user32.dll")] public static extern bool SetProcessDPIAware();
  [DllImport("user32.dll")] public static extern IntPtr GetForegroundWindow();
  [DllImport("user32.dll")] static extern bool EnumWindows(EnumWindowsProc cb, IntPtr p);
  [DllImport("user32.dll", CharSet=CharSet.Unicode)] static extern int GetWindowText(IntPtr h, StringBuilder s, int n);
  [DllImport("user32.dll")] static extern bool IsWindowVisible(IntPtr h);
  [DllImport("user32.dll")] public static extern bool GetClientRect(IntPtr h, out RECT r);
  [DllImport("user32.dll")] public static extern bool ClientToScreen(IntPtr h, ref POINT p);
  delegate bool EnumWindowsProc(IntPtr h, IntPtr p);
  [StructLayout(LayoutKind.Sequential)] public struct RECT { public int Left, Top, Right, Bottom; }
  [StructLayout(LayoutKind.Sequential)] public struct POINT { public int X, Y; }
  /// 按标题片段（不区分大小写）找第一个可见窗口
  public static IntPtr FindByTitle(string needle) {
    IntPtr found = IntPtr.Zero;
    EnumWindows((h, p) => {
      if (!IsWindowVisible(h)) return true;
      var sb = new StringBuilder(512);
      GetWindowText(h, sb, 512);
      if (sb.Length == 0) return true;
      if (sb.ToString().IndexOf(needle, StringComparison.OrdinalIgnoreCase) >= 0) { found = h; return false; }
      return true;
    }, IntPtr.Zero);
    return found;
  }
  /// 按窗口类名片段（不区分大小写）找第一个可见窗口。
  /// 类名比标题稳定：游戏切场景时常改标题（DELTARUNE 会变成 CONTACT），类名不变。
  public static IntPtr FindByClass(string needle) {
    IntPtr found = IntPtr.Zero;
    EnumWindows((h, p) => {
      if (!IsWindowVisible(h)) return true;
      var cls = new StringBuilder(256);
      GetClassName(h, cls, 256);
      if (cls.Length == 0) return true;
      if (cls.ToString().IndexOf(needle, StringComparison.OrdinalIgnoreCase) >= 0) { found = h; return false; }
      return true;
    }, IntPtr.Zero);
    return found;
  }
  [DllImport("user32.dll", CharSet=CharSet.Unicode)] static extern int GetClassName(IntPtr h, StringBuilder s, int n);
}
'@
    [void][WinOcr]::SetProcessDPIAware()
  } catch { }

  $source = 'full'
  $fallback = ''
  $windowName = ''

  if ($W -le 0 -or $H -le 0) {
    $bounds = [System.Windows.Forms.Screen]::PrimaryScreen.Bounds
    $X = $bounds.X; $Y = $bounds.Y; $W = $bounds.Width; $H = $bounds.Height

    # 类名优先：游戏切场景时会改标题（DELTARUNE → CONTACT），类名不变
    $hwnd = [IntPtr]::Zero
    $lookupKind = ''
    if ($WindowClass -ne '') {
      $hwnd = [WinOcr]::FindByClass($WindowClass)
      $lookupKind = "class:$WindowClass"
    }
    if ($hwnd -eq [IntPtr]::Zero -and $Window -ne '') {
      $hwnd = [WinOcr]::FindByTitle($Window)
      $lookupKind = "title:$Window"
    }

    if ($hwnd -ne [IntPtr]::Zero) {
      $r = New-Object WinOcr+RECT
      [void][WinOcr]::GetClientRect($hwnd, [ref]$r)
      $cw = $r.Right - $r.Left
      $ch = $r.Bottom - $r.Top
      if ($cw -ge 50 -and $ch -ge 50) {
        $pt = New-Object WinOcr+POINT
        $pt.X = 0; $pt.Y = 0
        [void][WinOcr]::ClientToScreen($hwnd, [ref]$pt)
        $X = $pt.X; $Y = $pt.Y; $W = $cw; $H = $ch
        $source = 'window'
        $windowName = $lookupKind
        # CopyFromScreen 抓的是「屏幕上那块矩形」，不是窗口自己的像素：
        # 目标窗口被别的窗口盖住时，会抓到盖在上面的内容。此时直接跳过这一拍。
        $fg = [WinOcr]::GetForegroundWindow()
        if ($fg -ne $hwnd) {
          [ordered]@{
            ok      = $true
            text    = ''
            w       = $W
            h       = $H
            ms      = [int]$sw.ElapsedMilliseconds
            source  = $source
            window  = $windowName
            skipped = 'not-foreground'
          } | ConvertTo-Json -Compress
          return
        }
      } else {
        $fallback = 'client-too-small'
      }
    } elseif ($WindowClass -ne '' -or $Window -ne '') {
      $fallback = 'window-not-found'
    }
  } else {
    $source = 'rect'
  }

  $bmp = New-Object System.Drawing.Bitmap($W, $H)
  $g = [System.Drawing.Graphics]::FromImage($bmp)
  $g.CopyFromScreen($X, $Y, 0, 0, (New-Object System.Drawing.Size($W, $H)))
  $g.Dispose()
  $bmp.Save($png, [System.Drawing.Imaging.ImageFormat]::Png)

  # 视觉模式：同一张位图顺便存一张 JPEG，避免二次截屏（省 250ms）
  $jpegOut = ''
  if ($Jpeg -ne '') {
    try {
      $enc = [System.Drawing.Imaging.ImageCodecInfo]::GetImageEncoders() | Where-Object { $_.MimeType -eq 'image/jpeg' }
      $ep = New-Object System.Drawing.Imaging.EncoderParameters(1)
      $ep.Param[0] = New-Object System.Drawing.Imaging.EncoderParameter([System.Drawing.Imaging.Encoder]::Quality, [int64]85)
      $bmp.Save($Jpeg, $enc, $ep)
      $jpegOut = $Jpeg
    } catch { }
  }
  $bmp.Dispose()

  $asTaskGeneric = ([System.WindowsRuntimeSystemExtensions].GetMethods() | Where-Object {
    $_.Name -eq 'AsTask' -and $_.GetParameters().Count -eq 1 -and $_.GetParameters()[0].ParameterType.Name -eq 'IAsyncOperation`1'
  })[0]
  function Await($op, $resultType) {
    $task = $asTaskGeneric.MakeGenericMethod($resultType).Invoke($null, @($op))
    $task.Wait(-1) | Out-Null
    $task.Result
  }

  [Windows.Storage.StorageFile, Windows.Storage, ContentType = WindowsRuntime] | Out-Null
  [Windows.Graphics.Imaging.BitmapDecoder, Windows.Graphics.Imaging, ContentType = WindowsRuntime] | Out-Null
  [Windows.Media.Ocr.OcrEngine, Windows.Foundation, ContentType = WindowsRuntime] | Out-Null
  [Windows.Globalization.Language, Windows.Globalization, ContentType = WindowsRuntime] | Out-Null

  $file = Await ([Windows.Storage.StorageFile]::GetFileFromPathAsync($png)) ([Windows.Storage.StorageFile])
  $stream = Await ($file.OpenAsync([Windows.Storage.FileAccessMode]::Read)) ([Windows.Storage.Streams.IRandomAccessStream])
  $decoder = Await ([Windows.Graphics.Imaging.BitmapDecoder]::CreateAsync($stream)) ([Windows.Graphics.Imaging.BitmapDecoder])
  $bitmap = Await ($decoder.GetSoftwareBitmapAsync()) ([Windows.Graphics.Imaging.SoftwareBitmap])

  $engine = [Windows.Media.Ocr.OcrEngine]::TryCreateFromLanguage((New-Object Windows.Globalization.Language($Lang)))
  if (-not $engine) {
    $alt = [Windows.Media.Ocr.OcrEngine]::AvailableRecognizerLanguages | Where-Object { $_.LanguageTag -like 'en*' } | Select-Object -First 1
    if ($alt) { $engine = [Windows.Media.Ocr.OcrEngine]::TryCreateFromLanguage($alt) }
  }
  if (-not $engine) { throw "no OCR engine for $Lang" }

  $result = Await ($engine.RecognizeAsync($bitmap)) ([Windows.Media.Ocr.OcrResult])
  $lines = @()
  foreach ($line in $result.Lines) { if ($line.Text.Trim() -ne '') { $lines += $line.Text.Trim() } }

  $payload = [ordered]@{
    ok     = $true
    text   = ($lines -join "`n")
    w      = $W
    h      = $H
    ms     = [int]$sw.ElapsedMilliseconds
    source = $source
  }
  if ($windowName -ne '') { $payload.window = $windowName }
  if ($jpegOut -ne '') { $payload.jpeg = $jpegOut }
  if ($fallback -ne '') { $payload.fallback = $fallback }
  $payload | ConvertTo-Json -Compress
}
catch {
  [ordered]@{ ok = $false; error = $_.Exception.Message; text = '' } | ConvertTo-Json -Compress
}
finally {
  if ($stream) { $stream.Dispose() }
  Remove-Item $png -Force -ErrorAction SilentlyContinue
}

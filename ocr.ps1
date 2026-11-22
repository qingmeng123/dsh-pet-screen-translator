# 截取屏幕指定区域并做 OCR，输出一行 JSON。
# 用法: pwsh -NoProfile -File ocr.ps1 [-X 0 -Y 0 -W 0 -H 0 -Lang en-GB]
# W/H 为 0 时取主屏全屏。输出: {"ok":true,"text":"...","w":1920,"h":1080,"ms":812}
param(
  [int]$X = 0,
  [int]$Y = 0,
  [int]$W = 0,
  [int]$H = 0,
  [string]$Lang = 'en-GB'
)

$ErrorActionPreference = 'Stop'
$sw = [System.Diagnostics.Stopwatch]::StartNew()
$png = Join-Path $env:TEMP ("dsh-st-ocr-{0}.png" -f [guid]::NewGuid().ToString('N'))
$stream = $null
try {
  Add-Type -AssemblyName System.Drawing
  Add-Type -AssemblyName System.Windows.Forms
  Add-Type -AssemblyName System.Runtime.WindowsRuntime

  $bounds = [System.Windows.Forms.Screen]::PrimaryScreen.Bounds
  if ($W -le 0 -or $H -le 0) { $X = $bounds.X; $Y = $bounds.Y; $W = $bounds.Width; $H = $bounds.Height }

  $bmp = New-Object System.Drawing.Bitmap($W, $H)
  $g = [System.Drawing.Graphics]::FromImage($bmp)
  $g.CopyFromScreen($X, $Y, 0, 0, (New-Object System.Drawing.Size($W, $H)))
  $g.Dispose()
  $bmp.Save($png, [System.Drawing.Imaging.ImageFormat]::Png)
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
    $fallback = [Windows.Media.Ocr.OcrEngine]::AvailableRecognizerLanguages | Where-Object { $_.LanguageTag -like 'en*' } | Select-Object -First 1
    if ($fallback) { $engine = [Windows.Media.Ocr.OcrEngine]::TryCreateFromLanguage($fallback) }
  }
  if (-not $engine) { throw "no OCR engine for $Lang" }

  $result = Await ($engine.RecognizeAsync($bitmap)) ([Windows.Media.Ocr.OcrResult])
  $lines = @()
  foreach ($line in $result.Lines) { if ($line.Text.Trim() -ne '') { $lines += $line.Text.Trim() } }

  $payload = [ordered]@{
    ok   = $true
    text = ($lines -join "`n")
    w    = $W
    h    = $H
    ms   = [int]$sw.ElapsedMilliseconds
  }
  $payload | ConvertTo-Json -Compress
}
catch {
  [ordered]@{ ok = $false; error = $_.Exception.Message; text = '' } | ConvertTo-Json -Compress
}
finally {
  if ($stream) { $stream.Dispose() }
  Remove-Item $png -Force -ErrorAction SilentlyContinue
}

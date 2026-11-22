# 只做 OCR：读一张已存在的图片文件并返回识别文本（不抓屏、不需要 DPI 感知）。
#
# 用法: powershell -NoProfile -ExecutionPolicy Bypass -File ocr-file.ps1 -Path x.jpg [-Lang en-GB]
# 输出: {"ok":true,"text":"...","ms":123}
#
# Go 侧负责抓屏与编码，这里只承担 Windows.Media.Ocr 这一段；两者解耦后，
# OCR 换实现（WinRT 原生绑定 / Tesseract）不影响调用方。
param(
  [Parameter(Mandatory = $true)][string]$Path,
  [string]$Lang = 'en-GB'
)

$ErrorActionPreference = 'Stop'
$sw = [System.Diagnostics.Stopwatch]::StartNew()
$stream = $null
try {
  Add-Type -AssemblyName System.Runtime.WindowsRuntime

  $asTaskGeneric = ([System.WindowsRuntimeSystemExtensions].GetMethods() | Where-Object {
    $_.Name -eq 'AsTask' -and $_.GetParameters().Count -eq 1 -and $_.GetParameters()[0].ParameterType.Name -eq 'IAsyncOperation`1'
  })[0]
  function Await($op, $resultType) {
    $task = $asTaskGeneric.MakeGenericMethod($resultType).Invoke($null, @($op))
    $task.Wait(-1) | Out-Null
    $task.Result
  }

  [Windows.Graphics.Imaging.BitmapDecoder, Windows.Graphics.Imaging, ContentType = WindowsRuntime] | Out-Null
  [Windows.Media.Ocr.OcrEngine, Windows.Foundation, ContentType = WindowsRuntime] | Out-Null
  [Windows.Globalization.Language, Windows.Globalization, ContentType = WindowsRuntime] | Out-Null

  # 不用 StorageFile：它只允许"应用可访问位置"（D:\... 会被拒绝访问）。
  # 改用 .NET FileStream + AsRandomAccessStream，任意路径都能读。
  $fs = [System.IO.File]::OpenRead($Path)
  $stream = [System.IO.WindowsRuntimeStreamExtensions]::AsRandomAccessStream($fs)
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

  [ordered]@{
    ok   = $true
    text = ($lines -join "`n")
    ms   = [int]$sw.ElapsedMilliseconds
  } | ConvertTo-Json -Compress
}
catch {
  $detail = $_.Exception.Message
  $base = $_.Exception.GetBaseException()
  if ($base -and $base.Message -ne $detail) { $detail = $detail + ' | base: ' + $base.Message }
  [ordered]@{ ok = $false; error = $detail; text = '' } | ConvertTo-Json -Compress
}
finally {
  if ($stream) { $stream.Dispose() }
}

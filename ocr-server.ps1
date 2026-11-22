param([string]$Lang = 'en-GB')

# 常驻 OCR 服务：从 stdin 逐行读 {"path":"..."}，把识别结果按行写回 stdout。
# 这样 PowerShell 启动 + WinRT 初始化只做一次，单次识别从 ~440ms 降到 ~150ms。
$ErrorActionPreference = 'Stop'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

Add-Type -AssemblyName System.Runtime.WindowsRuntime | Out-Null

$asTaskGeneric = ([System.WindowsRuntimeSystemExtensions].GetMethods() | Where-Object {
  $_.Name -eq 'AsTask' -and $_.GetParameters().Count -eq 1 -and
  $_.GetParameters()[0].ParameterType.Name -eq 'IAsyncOperation`1'
})[0]

function Await($op, $type) {
  $task = $asTaskGeneric.MakeGenericMethod($type).Invoke($null, @($op))
  $task.Wait()
  return $task.Result
}

[Windows.Globalization.Language, Windows.Foundation, ContentType = WindowsRuntime] | Out-Null
[Windows.Media.Ocr.OcrEngine, Windows.Foundation, ContentType = WindowsRuntime] | Out-Null
[Windows.Graphics.Imaging.BitmapDecoder, Windows.Foundation, ContentType = WindowsRuntime] | Out-Null

$language = New-Object Windows.Globalization.Language($Lang)
$engine = [Windows.Media.Ocr.OcrEngine]::TryCreateFromLanguage($language)
if ($null -eq $engine) {
  $engine = [Windows.Media.Ocr.OcrEngine]::TryCreateFromUserProfileLanguages()
}
if ($null -eq $engine) {
  [Console]::Out.WriteLine(([ordered]@{ ok = $false; error = 'no-ocr-engine'; text = '' } | ConvertTo-Json -Compress))
  [Console]::Out.Flush()
  exit 1
}

# 就绪信号：Go 侧读到这一行才算启动成功
[Console]::Out.WriteLine(([ordered]@{ ready = $true; lang = $Lang } | ConvertTo-Json -Compress))
[Console]::Out.Flush()

while ($true) {
  $line = [Console]::In.ReadLine()
  if ($null -eq $line) { break }
  $line = $line.Trim()
  if ($line -eq '') { continue }
  if ($line -eq '{"quit":true}') { break }

  $sw = [System.Diagnostics.Stopwatch]::StartNew()
  try {
    $req = $line | ConvertFrom-Json
    $path = [string]$req.path
    $fs = [System.IO.File]::OpenRead($path)
    $stream = [System.IO.WindowsRuntimeStreamExtensions]::AsRandomAccessStream($fs)
    $decoder = Await ([Windows.Graphics.Imaging.BitmapDecoder]::CreateAsync($stream)) ([Windows.Graphics.Imaging.BitmapDecoder])
    $bitmap = Await ($decoder.GetSoftwareBitmapAsync()) ([Windows.Graphics.Imaging.SoftwareBitmap])
    $result = Await ($engine.RecognizeAsync($bitmap)) ([Windows.Media.Ocr.OcrResult])
    $text = (($result.Lines | ForEach-Object { $_.Text }) -join "`n")
    $bitmap.Dispose()
    $stream.Dispose()
    $fs.Dispose()
    $resp = [ordered]@{ ok = $true; text = $text; ms = [int]$sw.ElapsedMilliseconds } | ConvertTo-Json -Compress
  } catch {
    $resp = [ordered]@{ ok = $false; error = $_.Exception.Message; text = ''; ms = [int]$sw.ElapsedMilliseconds } | ConvertTo-Json -Compress
  }
  [Console]::Out.WriteLine($resp)
  [Console]::Out.Flush()
}

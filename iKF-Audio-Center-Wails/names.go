// 属性名表 —— S3 起真源 = model/device-model.toml [names]
// （208 = generic 112 + oem 96，装载时自校验）。
// 表归属决定 pValue 约定，务必与 Python 版逐项一致；顺序即下标。

package main

var genericNames = modelData.Names.Generic

var oemNamesList = modelData.Names.Oem

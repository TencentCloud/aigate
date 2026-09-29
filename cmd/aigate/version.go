//
//
// Tencent is pleased to support the open source community by making Tencent Cloud AIGate available.
//
// Copyright (C) 2026 Tencent.
// All rights reserved.
//
// If you have downloaded a copy of the Tencent Cloud AIGate source code from Tencent,
// please note that Tencent Cloud AIGate source code is licensed under the Apache 2.0 License,
// A copy of the Apache 2.0 License is included in this file.
//
//

package main

import "fmt"

// Version 是网关版本号：能力变更时与 CHANGELOG.md 同步递增。
const Version = "0.1.0"

// printVersion 输出版本行（`-version` 的唯一输出，随后退出）。
func printVersion() {
	fmt.Printf("Tencent Cloud AIGate %s\n", Version)
}

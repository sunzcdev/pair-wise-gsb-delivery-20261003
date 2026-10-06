// Copyright 2026 Matthew Holt
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly || solaris || aix)

package acmez

import "github.com/miekg/dns"

// systemDNSConfig reports no system configuration on platforms without a
// resolv.conf (notably Windows). The DNSCAAQuerier then falls back to its
// built-in public resolvers; callers needing the OS resolver should set
// DNSCAAQuerier.Config explicitly.
func systemDNSConfig() *dns.ClientConfig { return nil }

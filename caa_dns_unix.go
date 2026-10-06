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

//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly || solaris || aix

package acmez

import "github.com/miekg/dns"

// systemDNSConfig reads the system resolver configuration from
// /etc/resolv.conf, which is available (or emulated) on the supported
// Unix-like platforms. It returns nil if the file cannot be read or names no
// servers, in which case the caller falls back to public resolvers.
func systemDNSConfig() *dns.ClientConfig {
	cfg, err := dns.ClientConfigFromFile("/etc/resolv.conf")
	if err != nil || cfg == nil || len(cfg.Servers) == 0 {
		return nil
	}
	return cfg
}

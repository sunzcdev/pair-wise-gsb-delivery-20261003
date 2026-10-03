// Command arc-relay-demo constructs a two-hop ARC flow and shows where the
// chain breaks under three tampering scenarios. It uses in-memory keys (no
// DNS) and prints results in Chinese.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"strings"

	"github.com/emersion/go-msgauth/arc"
	"github.com/emersion/go-msgauth/authres"
	"github.com/emersion/go-msgauth/dkim"
)

type relay struct {
	domain string
	priv   ed25519.PrivateKey
}

type memoryKeys struct {
	pubs map[string]ed25519.PublicKey
}

func (m *memoryKeys) LookupARCKey(domain, selector string) (*dkim.PublicKey, error) {
	if pub, ok := m.pubs[domain]; ok {
		return dkim.NewPublicKey(pub)
	}
	return nil, nil
}

func main() {
	relay1 := newRelay("mx.relay1.example")
	relay2 := newRelay("mx.relay2.example")
	keys := &memoryKeys{pubs: map[string]ed25519.PublicKey{
		relay1.domain: relay1.priv.Public().(ed25519.PublicKey),
		relay2.domain: relay2.priv.Public().(ed25519.PublicKey),
	}}

	incoming := "From: alice@example.com\r\n" +
		"To: news@relay2.example\r\n" +
		"Subject: weekly digest\r\n" +
		"X-Hop1-Only: 只在第一跳存在的中转备注\r\n" +
		"\r\n" +
		"原始正文（第一跳收到的样子）\r\n"

	fmt.Println("========== 正常的两跳中转 ==========")

	// 第一跳：认证入站邮件并加封（实例 1，cv=none）。
	hop1 := seal(relay1, keys, incoming, arc.ChainValidationNone,
		[]authres.Result{
			&authres.DKIMResult{Value: authres.ResultPass, Domain: "example.com"},
			&authres.SPFResult{Value: authres.ResultPass, From: "example.com"},
		})
	report("第一跳之后", verify(hop1, keys))

	// 第二跳：先验证第一跳的链（决定 cv），修改正文，再加封（实例 2）。
	preCheck := verify(hop1, keys)
	cv := arc.ChainValidationFail
	if preCheck.Result == arc.ChainPass {
		cv = arc.ChainValidationPass
	}
	rewritten := strings.Replace(hop1,
		"原始正文（第一跳收到的样子）\r\n",
		"正文已被第二跳的邮件列表改写，旧 DKIM/AMS 的 body hash 因此失效\r\n", 1)
	hop2 := sealKeys(relay2, keys, rewritten, cv,
		[]string{"From", "To", "Subject"},
		[]authres.Result{
			&authres.DKIMResult{Value: authres.ResultFail, Domain: "example.com",
				Reason: "body changed by mailing list"},
			&authres.DMARCResult{Value: authres.ResultPass, From: "example.com"},
		})
	v := report("第二跳之后", verify(hop2, keys))

	fmt.Println()
	fmt.Println("调用方策略：密码学链有效 ≠ 可放行。")
	switch {
	case v.Trusted([]string{"mx.relay2.example"}):
		fmt.Println("  最新封印域 mx.relay2.example 在信任名单中：可以采信其 AAR，再按本地 SPF/DKIM/DMARC 策略决定。")
	default:
		fmt.Println("  最新封印域不在信任名单中：即使链通过也不采信。")
	}

	fmt.Println()
	fmt.Println("========== 三种篡改：链分别在哪一步失效 ==========")

	// 场景 1：篡改旧封印（AS(i=1)）。它被 AS(i=2) 的签名覆盖，
	// 所以结构检查能通过，失败发生在校验第二跳封印时。
	tamperedSeal := strings.Replace(hop2,
		"cv=none;", "x=injected; cv=none;", 1)
	report("篡改旧封印 AS(i=1)", verify(tamperedSeal, keys))

	// 场景 2：删除一组头。X-Hop1-Only 只被旧 AMS(i=1) 覆盖，第二跳的
	// AMS(i=2) 没有签它。删除后最新 AMS 仍有效，但旧 AMS 头签名失效，
	// 失败体现为 AS(i=2) 的 cv=pass 与实际情况不符。
	deletedHeader := strings.Replace(hop2, "X-Hop1-Only: 只在第一跳存在的中转备注\r\n", "", 1)
	report("删除一组头（X-Hop1-Only）", verify(deletedHeader, keys))

	// 场景 3：重复实例号。复制一份 AS(i=1)，链在结构校验阶段即失败。
	duplicate := insertCopy(hop2, "ARC-Seal: a=ed25519-sha256; cv=none;")
	report("重复实例号（再插一个 AS(i=1)）", verify(duplicate, keys))
}

func newRelay(domain string) *relay {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	_ = pub
	return &relay{domain: domain, priv: priv}
}

func seal(r *relay, keys arc.KeyResolver, msg string, cv arc.ChainValidationValue, results []authres.Result) string {
	return sealKeys(r, keys, msg, cv, nil, results)
}

func sealKeys(r *relay, keys arc.KeyResolver, msg string, cv arc.ChainValidationValue, headerKeys []string, results []authres.Result) string {
	var b strings.Builder
	err := arc.Seal(&b, strings.NewReader(msg), &arc.SealOptions{
		Domain:               r.domain,
		Selector:             "arc",
		Signer:               r.priv,
		AuthenticationServID: r.domain,
		AuthResults:          results,
		CV:                   cv,
		Resolver:             keys,
		HeaderKeys:           headerKeys,
	})
	if err != nil {
		panic(err)
	}
	return b.String()
}

func verify(msg string, keys arc.KeyResolver) *arc.Verification {
	v, err := arc.VerifyWithOptions(strings.NewReader(msg), &arc.VerifyOptions{Resolver: keys})
	if err != nil {
		panic(err)
	}
	return v
}

func report(label string, v *arc.Verification) *arc.Verification {
	status := map[arc.ChainResult]string{
		arc.ChainNone: "无 ARC 集",
		arc.ChainPass: "链有效 (pass)",
		arc.ChainFail: "链失效 (fail)",
	}[v.Result]
	fmt.Printf("%-32s 实例数=%d  结果：%s\n", label, v.Instances(), status)
	if v.Err != nil {
		fmt.Printf("    失效步骤：%v\n", v.Err)
	}
	for _, set := range v.Sets {
		flags := []string{}
		if set.AMSErr != nil {
			flags = append(flags, "AMS头签名无效")
		}
		if set.AMSBodyErr != nil {
			flags = append(flags, "AMS正文哈希失效(旧实例允许)")
		}
		if set.ASErr != nil {
			flags = append(flags, "AS封印无效")
		}
		if len(flags) > 0 {
			fmt.Printf("      i=%d (%s): %s\n", set.Instance, set.Domain, strings.Join(flags, "；"))
		}
	}
	return v
}

func insertCopy(msg, prefix string) string {
	idx := strings.Index(msg, prefix)
	if idx < 0 {
		return msg
	}
	end := strings.Index(msg[idx:], "\r\n")
	if end < 0 {
		return msg
	}
	end = idx + end + 2
	return msg[:idx] + msg[idx:end] + msg[idx:]
}

// This example resolves openid_provider metadata through a local, in-memory
// OpenID Federation graph. It needs no network access: the [federation.Client]
// is replaced by [federation.LocalClient].
//
// Run it with:
//
//	go run ./federation/example_local
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/federation"
)

const (
	anchorID = "https://trust-anchor.example"
	midID    = "https://intermediate.example"
	leafID   = "https://op.example"
	otherID  = "https://other-federation.example"
	rogueID  = "https://rogue-federation.example"
)

func main() {
	ctx := context.Background()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

	fmt.Println(strings.Repeat("=", 78))
	fmt.Println("场景 1：三层实体（信任锚 -> 中间实体 -> OP），策略逐级合并")
	fmt.Println(strings.Repeat("=", 78))
	runScenario1(ctx, now)

	fmt.Println()
	fmt.Println(strings.Repeat("=", 78))
	fmt.Println("场景 2：冲突的 metadata policy（下游 value 不在上游 subset_of 内，必须报错）")
	fmt.Println(strings.Repeat("=", 78))
	runScenario2(ctx, now)

	fmt.Println()
	fmt.Println(strings.Repeat("=", 78))
	fmt.Println("场景 3：关键操作符不在支持范围内（less_than），必须拒绝，不得忽略")
	fmt.Println(strings.Repeat("=", 78))
	runScenario3(ctx, now)

	fmt.Println()
	fmt.Println(strings.Repeat("=", 78))
	fmt.Println("场景 4：断链（中间实体的 subordinate statement 取不到）")
	fmt.Println(strings.Repeat("=", 78))
	runScenario4(ctx, now)

	fmt.Println()
	fmt.Println(strings.Repeat("=", 78))
	fmt.Println("场景 5：环 + 多条路径（发现 URL 不是信任锚，选最短可信路径）")
	fmt.Println(strings.Repeat("=", 78))
	runScenario5(ctx, now)
}

func runScenario1(ctx context.Context, now time.Time) {
	client, _ := buildThreeTier(now, threeTierOptions{})

	resolver := federation.NewResolver(client, anchorID).WithClock(constantClock(now))
	result, err := resolver.ResolveProvider(ctx, leafID)
	must(err)

	printResult(result)
	fmt.Printf("信任锚配置了 %q；叶子发现 URL %q 本身从未被当作信任锚。\n", anchorID, leafID)
}

func runScenario2(ctx context.Context, now time.Time) {
	client, _ := buildThreeTier(now, threeTierOptions{conflict: true})

	resolver := federation.NewResolver(client, anchorID).WithClock(constantClock(now))
	_, err := resolver.ResolveProvider(ctx, leafID)
	printFailure(err)
}

func runScenario3(ctx context.Context, now time.Time) {
	client, _ := buildThreeTier(now, threeTierOptions{unsupportedOperator: true})

	resolver := federation.NewResolver(client, anchorID).WithClock(constantClock(now))
	_, err := resolver.ResolveProvider(ctx, leafID)
	printFailure(err)
}

func runScenario4(ctx context.Context, now time.Time) {
	client, _ := buildThreeTier(now, threeTierOptions{brokenEdge: true})

	resolver := federation.NewResolver(client, anchorID).WithClock(constantClock(now))
	_, err := resolver.ResolveProvider(ctx, leafID)
	printFailure(err)

	if errors.Is(err, federation.ErrNoTrustPath) {
		fmt.Println("确认：返回的是 ErrNoTrustPath，而不是把缺失环节静默跳过。")
	}
}

func runScenario5(ctx context.Context, now time.Time) {
	client := buildCycleAndPaths(now)

	// 注意：信任锚只有 anchorID。otherID/rogueID 即使能形成路径，也不被信任。
	resolver := federation.NewResolver(client, anchorID).WithClock(constantClock(now))
	result, err := resolver.ResolveProvider(ctx, leafID)
	must(err)

	printResult(result)
	fmt.Println("发现过程（含被拒绝的环和非信任锚路径）：")
	for _, attempt := range result.Trace.Attempts {
		fmt.Printf("  尝试 %s -> 拒绝：%v\n", strings.Join(attempt.Path, " -> "), attempt.Reason)
	}
	fmt.Println("所有验证通过的路径：")
	for i, path := range result.Trace.ValidPaths {
		fmt.Printf("  %d. %s\n", i+1, strings.Join(path, " -> "))
	}
}

type threeTierOptions struct {
	conflict            bool
	unsupportedOperator bool
	brokenEdge          bool
}

func buildThreeTier(now time.Time, opts threeTierOptions) (*federation.LocalClient, map[string]*federation.Entity) {
	client := federation.NewLocalClient()

	entities, err := federation.AddEntities(client, now,
		&federation.EntityInput{
			ID: anchorID,
			FederationMetadata: []byte(
				`{"federation_fetch_endpoint":"https://trust-anchor.example/fetch"}`),
		},
		&federation.EntityInput{
			ID:             midID,
			AuthorityHints: []string{anchorID},
			FederationMetadata: []byte(
				`{"federation_fetch_endpoint":"https://intermediate.example/fetch"}`),
		},
		&federation.EntityInput{
			ID:             leafID,
			AuthorityHints: []string{midID},
			ProviderMetadata: []byte(`{
				"issuer": "https://op.example",
				"authorization_endpoint": "https://op.example/authorize",
				"token_endpoint": "https://op.example/token",
				"jwks_uri": "https://op.example/jwks",
				"scopes_supported": ["openid"],
				"grant_types_supported": ["authorization_code", "refresh_token"],
				"request_parameter_supported": true
			}`),
		},
	)
	must(err)

	// 信任锚关于中间实体的策略：限定 scope 集合、强制 code challenge 方法、
	// 对 contacts 给出默认值。
	anchorPolicy := []byte(`{
		"scopes_supported": {"subset_of": ["openid", "profile", "email", "offline_access"]},
		"code_challenge_methods_supported": {"value": ["S256"]},
		"contacts": {"default": ["ops@trust-anchor.example"]}
	}`)
	if opts.conflict {
		anchorPolicy = []byte(`{
			"scopes_supported": {"subset_of": ["openid", "profile"]}
		}`)
	}

	// 中间实体关于 OP 的策略：在允许集合内追加 scope，要求离线访问能力，
	// 限定 request_parameter_supported 的取值。
	midPolicy := []byte(`{
		"scopes_supported": {"add": ["profile", "email"]},
		"grant_types_supported": {"superset_of": ["authorization_code", "refresh_token"]},
		"request_parameter_supported": {"one_of": [true]}
	}`)

	if opts.conflict {
		// 信任锚把 scope 限定在 {openid,profile} 内，中间实体却用 value
		// 把 scopes_supported 固定成不在该集合内的 ["email"]：两条规则
		// 无法同时成立，必须报错，不能让 value 静默覆盖上游约束。
		midPolicy = []byte(`{
			"scopes_supported": {"value": ["email"]}
		}`)
	}
	if opts.unsupportedOperator {
		// less_than 不属于本解析器声明的支持集合，出现在 openid_provider
		// 策略上必须整条链失败。
		midPolicy = []byte(`{
			"authorization_endpoint": {"less_than": 1024}
		}`)
	}

	_, err = federation.AddStatement(client, entities[anchorID], &federation.StatementInput{
		Subject:        midID,
		ProviderPolicy: anchorPolicy,
	}, now)
	must(err)

	_, err = federation.AddStatement(client, entities[midID], &federation.StatementInput{
		Subject:        leafID,
		ProviderPolicy: midPolicy,
	}, now)
	must(err)

	if opts.brokenEdge {
		// 中间实体拿不出关于 OP 的 subordinate statement（404 等价）。
		client.BreakStatement(midID, leafID)
	}

	return client, entities
}

func buildCycleAndPaths(now time.Time) *federation.LocalClient {
	client := federation.NewLocalClient()

	entities, err := federation.AddEntities(client, now,
		&federation.EntityInput{ID: anchorID, FederationMetadata: []byte(
			`{"federation_fetch_endpoint":"https://trust-anchor.example/fetch"}`)},
		&federation.EntityInput{ID: otherID, AuthorityHints: []string{anchorID}, FederationMetadata: []byte(
			`{"federation_fetch_endpoint":"https://other-federation.example/fetch"}`)},
		&federation.EntityInput{ID: rogueID, FederationMetadata: []byte(
			`{"federation_fetch_endpoint":"https://rogue-federation.example/fetch"}`)},
		&federation.EntityInput{
			ID:             midID,
			AuthorityHints: []string{anchorID, otherID, leafID},
			ProviderMetadata: []byte(`{
				"issuer": "https://intermediate.example",
				"scopes_supported": ["openid"]
			}`),
		},
		&federation.EntityInput{
			ID:             leafID,
			AuthorityHints: []string{midID, rogueID},
			ProviderMetadata: []byte(`{
				"issuer": "https://op.example",
				"authorization_endpoint": "https://op.example/authorize",
				"token_endpoint": "https://op.example/token",
				"jwks_uri": "https://op.example/jwks",
				"scopes_supported": ["openid", "profile"]
			}`),
		},
	)
	must(err)

	// 直接的短链：anchor -> mid -> leaf（leaf 先按 mid 走，mid 再到 anchor）。
	mustAdd(client, entities[anchorID], midID,
		`{"scopes_supported":{"subset_of":["openid","profile","email"]}}`, now)
	mustAdd(client, entities[midID], leafID,
		`{"scopes_supported":{"add":["email"]}}`, now)

	// 较长的替代链：anchor -> other -> mid；以及 rogue -> leaf（rogue 不是
	// 信任锚，此路径不可信）。
	mustAdd(client, entities[anchorID], otherID,
		`{"scopes_supported":{"subset_of":["openid","profile"]}}`, now)
	mustAdd(client, entities[otherID], midID, `{}`, now)
	mustAdd(client, entities[rogueID], leafID,
		`{"scopes_supported":{"value":["attacker-scope"]}}`, now)

	// mid 的 authority_hints 里故意放入 leaf，制造 mid -> leaf -> mid 的环。

	return client
}

func mustAdd(client *federation.LocalClient, issuer *federation.Entity, subject, policy string, now time.Time) {
	_, err := federation.AddStatement(client, issuer, &federation.StatementInput{
		Subject:        subject,
		ProviderPolicy: []byte(policy),
	}, now)
	must(err)
}

func printResult(result *federation.Result) {
	fmt.Println("选中的可信链（叶子 -> 信任锚）：")
	for i, id := range result.Chain.Entities() {
		marker := "叶子"
		if i == len(result.Chain.Entities())-1 {
			marker = "信任锚"
		} else if i > 0 {
			marker = "中间实体"
		}
		fmt.Printf("  %d. %s  (%s)\n", i+1, id, marker)
	}

	fmt.Println("链上策略贡献（信任锚 -> 叶子）：")
	for _, c := range result.PolicyContributions {
		if len(c.Policy) == 0 {
			fmt.Printf("  %s -> %s: （无 openid_provider 策略）\n", c.Issuer, c.Subject)
			continue
		}
		params := make([]string, 0, len(c.Policy))
		for param := range c.Policy {
			params = append(params, param)
		}
		fmt.Printf("  %s -> %s: %s\n", c.Issuer, c.Subject, strings.Join(params, ", "))
	}

	fmt.Println("最终可信 openid_provider 元数据：")
	fmt.Println(indentJSON(result.ProviderMetadata))

	fmt.Println("最终可信元数据的来源：")
	origins := append([]federation.ClaimOrigin(nil), result.Origins...)
	sortOrigins(origins)
	for _, origin := range origins {
		operator := origin.Operator
		if operator == "" {
			operator = "未被策略修改"
		}
		fmt.Printf("  %-34s 来源=%-34s 操作符=%s\n", origin.Parameter, origin.Source, operator)
	}
}

func printFailure(err error) {
	fmt.Printf("解析被拒绝：%v\n", err)
}

func sortOrigins(origins []federation.ClaimOrigin) {
	for i := 1; i < len(origins); i++ {
		for j := i; j > 0 && origins[j-1].Parameter > origins[j].Parameter; j-- {
			origins[j-1], origins[j] = origins[j], origins[j-1]
		}
	}
}

func indentJSON(raw []byte) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "  ", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}

func constantClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "example setup failed:", err)
		os.Exit(1)
	}
}

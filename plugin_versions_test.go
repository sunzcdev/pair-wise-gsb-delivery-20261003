// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"net/rpc"
	"testing"
	"time"
)

// testServiceInterface is implemented independently by the "storage" and
// "auth" test services. Scale returns a version dependent multiplier so tests
// can assert which implementation was registered after negotiation.
type testServiceInterface interface {
	Scale(int) int
}

// testVersionedServicePlugin is a net/rpc Plugin for a versioned test service.
type testVersionedServicePlugin struct {
	name    string
	version int
}

func (p *testVersionedServicePlugin) Server(*MuxBroker) (interface{}, error) {
	return &testVersionedServiceServer{name: p.name, version: p.version}, nil
}

func (p *testVersionedServicePlugin) Client(_ *MuxBroker, c *rpc.Client) (interface{}, error) {
	return &testVersionedServiceClient{name: p.name, Client: c}, nil
}

// testVersionedServiceServer is the net/rpc server side. Version 1 doubles the
// argument, version 2 quadruples it; storage and auth also use distinct offsets
// so both name and version are observable end to end.
type testVersionedServiceServer struct {
	name    string
	version int
}

func (s *testVersionedServiceServer) Scale(arg int, resp *int) error {
	multiplier := s.version * 2
	if s.name == "auth" {
		multiplier += 6
	}
	*resp = arg * multiplier
	return nil
}

// testVersionedServiceClient calls the per-service RPC method.
type testVersionedServiceClient struct {
	name   string
	Client *rpc.Client
}

func (c *testVersionedServiceClient) Scale(arg int) int {
	var resp int
	if err := c.Client.Call("Plugin.Scale", arg, &resp); err != nil {
		panic(err)
	}
	return resp
}

const (
	testStoragePlugin = "storage"
	testAuthPlugin    = "auth"
)

func servicePlugin(name string, version int) Plugin {
	return &testVersionedServicePlugin{name: name, version: version}
}

func pluginVersion(name string, version int, required bool) PluginVersion {
	return PluginVersion{Plugin: servicePlugin(name, version), Required: required}
}

// requiredServerServiceVersions is the server catalog: both services offer
// versions 1 and 2 and are required.
func requiredServerServiceVersions() PluginVersions {
	return PluginVersions{
		testStoragePlugin: {
			1: pluginVersion(testStoragePlugin, 1, true),
			2: pluginVersion(testStoragePlugin, 2, true),
		},
		testAuthPlugin: {
			1: pluginVersion(testAuthPlugin, 1, true),
			2: pluginVersion(testAuthPlugin, 2, true),
		},
	}
}

// optionalServerServiceVersions marks the auth service optional.
func optionalServerServiceVersions() PluginVersions {
	catalog := requiredServerServiceVersions()
	byVersion := catalog[testAuthPlugin]
	byVersion[1] = pluginVersion(testAuthPlugin, 1, false)
	byVersion[2] = pluginVersion(testAuthPlugin, 2, false)
	return catalog
}

func legacyServicePluginMap() PluginSet {
	return PluginSet{
		testStoragePlugin: servicePlugin(testStoragePlugin, 1),
		testAuthPlugin:    servicePlugin(testAuthPlugin, 1),
	}
}

func clientServiceVersions(storageVersions, authVersions []int, storageRequired, authRequired bool) PluginVersions {
	catalog := PluginVersions{
		testStoragePlugin: make(map[int]PluginVersion, len(storageVersions)),
		testAuthPlugin:    make(map[int]PluginVersion, len(authVersions)),
	}
	for _, v := range storageVersions {
		catalog[testStoragePlugin][v] = pluginVersion(testStoragePlugin, v, storageRequired)
	}
	for _, v := range authVersions {
		catalog[testAuthPlugin][v] = pluginVersion(testAuthPlugin, v, authRequired)
	}
	return catalog
}

// scaleResult is what a negotiated service returns for input 1:
// storage v1 -> 2, storage v2 -> 4, auth v1 -> 8, auth v2 -> 10.
const (
	storageV1Result = 2
	storageV2Result = 4
	authV1Result    = 8
	authV2Result    = 10
)

func dispenseService(t *testing.T, client ClientProtocol, name string, want int) {
	t.Helper()

	raw, err := client.Dispense(name)
	if err != nil {
		t.Fatalf("Dispense(%q) error: %s", name, err)
	}
	service, ok := raw.(testServiceInterface)
	if !ok {
		t.Fatalf("Dispense(%q) returned %T, want testServiceInterface", name, raw)
	}
	if got := service.Scale(1); got != want {
		t.Fatalf("%s.Scale = %d, want %d", name, got, want)
	}
}

func newServiceClient(t *testing.T, helper string, catalog PluginVersions) *Client {
	t.Helper()
	return NewClient(&ClientConfig{
		Cmd:             helperProcess(helper),
		HandshakeConfig: testVersionedHandshake,
		PluginVersions:  catalog,
		StartTimeout:    5 * time.Second,
	})
}

// TestPluginVersions_fullMatrix verifies the highest mutually supported
// version is chosen independently for each service, across the two-services
// two-versions matrix.
func TestPluginVersions_fullMatrix(t *testing.T) {
	cases := []struct {
		name           string
		clientVersions PluginVersions
		wantStorage    int
		wantAuth       int
	}{
		{
			name:           "all versions supported",
			clientVersions: clientServiceVersions([]int{1, 2}, []int{1, 2}, true, true),
			wantStorage:    storageV2Result,
			wantAuth:       authV2Result,
		},
		{
			name:           "independent cadences",
			clientVersions: clientServiceVersions([]int{1}, []int{2}, true, true),
			wantStorage:    storageV1Result,
			wantAuth:       authV2Result,
		},
		{
			name:           "split versions",
			clientVersions: clientServiceVersions([]int{1, 2}, []int{1}, true, true),
			wantStorage:    storageV2Result,
			wantAuth:       authV1Result,
		},
		{
			name:           "oldest common",
			clientVersions: clientServiceVersions([]int{1}, []int{1}, true, true),
			wantStorage:    storageV1Result,
			wantAuth:       authV1Result,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newServiceClient(t, "test-service-versions", tc.clientVersions)
			defer c.Kill()

			if _, err := c.Start(); err != nil {
				t.Fatalf("Start error: %s", err)
			}

			if got := c.NegotiatedVersions()[testStoragePlugin]; got != tc.wantStorage/2 {
				t.Fatalf("negotiated storage version = %d, want %d", got, tc.wantStorage/2)
			}
			if got := c.NegotiatedVersions()[testAuthPlugin]; got != (tc.wantAuth-6)/2 {
				t.Fatalf("negotiated auth version = %d, want %d", got, (tc.wantAuth-6)/2)
			}

			client, err := c.Client()
			if err != nil {
				t.Fatalf("Client error: %s", err)
			}

			// The dispensed implementations must match the negotiated versions.
			dispenseService(t, client, testStoragePlugin, tc.wantStorage)
			dispenseService(t, client, testAuthPlugin, tc.wantAuth)
		})
	}
}

// TestPluginVersions_optionalMissing verifies that an optional service with no
// client-side support is dropped rather than failing startup, while the
// required storage service still negotiates.
func TestPluginVersions_optionalMissing(t *testing.T) {
	catalog := clientServiceVersions([]int{1, 2}, nil, true, false)
	c := newServiceClient(t, "test-service-versions-optional", catalog)
	defer c.Kill()

	if _, err := c.Start(); err != nil {
		t.Fatalf("Start error: %s", err)
	}

	versions := c.NegotiatedVersions()
	if _, ok := versions[testAuthPlugin]; ok {
		t.Fatal("optional auth should be absent from negotiated versions")
	}
	if versions[testStoragePlugin] != 2 {
		t.Fatalf("negotiated storage version = %d, want 2", versions[testStoragePlugin])
	}

	client, err := c.Client()
	if err != nil {
		t.Fatalf("Client error: %s", err)
	}
	dispenseService(t, client, testStoragePlugin, storageV2Result)

	if _, err := client.Dispense(testAuthPlugin); err == nil {
		t.Fatal("Dispense on omitted optional service should fail")
	}
}

// TestPluginVersions_optionalVersionMismatch drops an optional service when its
// version ranges do not overlap, but startup still succeeds.
func TestPluginVersions_optionalVersionMismatch(t *testing.T) {
	// Client offers auth at unsupported version 3 only; keep it optional.
	catalog := clientServiceVersions([]int{1, 2}, []int{3}, true, false)
	c := newServiceClient(t, "test-service-versions-optional", catalog)
	defer c.Kill()

	if _, err := c.Start(); err != nil {
		t.Fatalf("Start error: %s", err)
	}

	versions := c.NegotiatedVersions()
	if _, ok := versions[testAuthPlugin]; ok {
		t.Fatal("optional auth with no common version should be absent")
	}

	client, err := c.Client()
	if err != nil {
		t.Fatalf("Client error: %s", err)
	}
	dispenseService(t, client, testStoragePlugin, storageV2Result)
}

// TestPluginVersions_requiredMissingClient fails at startup when a service the
// server requires is not offered by the client.
func TestPluginVersions_requiredMissingClient(t *testing.T) {
	catalog := clientServiceVersions([]int{1, 2}, nil, true, false)
	c := newServiceClient(t, "test-service-versions", catalog)
	defer c.Kill()

	if _, err := c.Start(); err == nil {
		t.Fatal("expected startup failure when server-required auth is missing")
	}
}

// TestPluginVersions_requiredVersionMismatch fails at startup when a required
// service has no overlapping version.
func TestPluginVersions_requiredVersionMismatch(t *testing.T) {
	catalog := clientServiceVersions([]int{1, 2}, []int{3}, true, true)
	c := newServiceClient(t, "test-service-versions", catalog)
	defer c.Kill()

	if _, err := c.Start(); err == nil {
		t.Fatal("expected startup failure for required auth with no common version")
	}
}

// TestPluginVersions_legacyServer verifies the predictable compatibility
// result against an old server that only negotiates one global version (1).
func TestPluginVersions_legacyServer(t *testing.T) {
	t.Run("required services supported at global version", func(t *testing.T) {
		catalog := clientServiceVersions([]int{1, 2}, []int{1, 2}, true, true)
		c := newServiceClient(t, "test-service-versions-global", catalog)
		defer c.Kill()

		if _, err := c.Start(); err != nil {
			t.Fatalf("Start error: %s", err)
		}

		if c.NegotiatedVersion() != 1 {
			t.Fatalf("global version = %d, want 1", c.NegotiatedVersion())
		}
		versions := c.NegotiatedVersions()
		if versions[testStoragePlugin] != 1 || versions[testAuthPlugin] != 1 {
			t.Fatalf("negotiated versions = %v, want both at 1", versions)
		}

		client, err := c.Client()
		if err != nil {
			t.Fatalf("Client error: %s", err)
		}
		dispenseService(t, client, testStoragePlugin, storageV1Result)
		dispenseService(t, client, testAuthPlugin, authV1Result)
	})

	t.Run("required service unsupported at global version", func(t *testing.T) {
		// Client only knows storage v2 and auth v2; the global server speaks 1.
		catalog := clientServiceVersions([]int{2}, []int{2}, true, true)
		c := newServiceClient(t, "test-service-versions-global", catalog)
		defer c.Kill()

		if _, err := c.Start(); err == nil {
			t.Fatal("expected startup failure: required services lack global version 1")
		}
	})

	t.Run("optional service unsupported at global version", func(t *testing.T) {
		// Storage supports 1, auth only supports 2 and is optional: storage is
		// served, auth is predictably dropped.
		catalog := clientServiceVersions([]int{1, 2}, []int{2}, true, false)
		c := newServiceClient(t, "test-service-versions-global", catalog)
		defer c.Kill()

		if _, err := c.Start(); err != nil {
			t.Fatalf("Start error: %s", err)
		}

		versions := c.NegotiatedVersions()
		if versions[testStoragePlugin] != 1 {
			t.Fatalf("storage version = %d, want 1", versions[testStoragePlugin])
		}
		if _, ok := versions[testAuthPlugin]; ok {
			t.Fatal("optional auth should be dropped under global version 1")
		}

		client, err := c.Client()
		if err != nil {
			t.Fatalf("Client error: %s", err)
		}
		dispenseService(t, client, testStoragePlugin, storageV1Result)
	})
}

// TestPluginVersions_encodeParse round trips the environment catalog and
// negotiated segment encoding.
func TestPluginVersions_encodeParse(t *testing.T) {
	catalog := PluginVersions{
		testAuthPlugin: {
			2: pluginVersion(testAuthPlugin, 2, false),
		},
		testStoragePlugin: {
			1: pluginVersion(testStoragePlugin, 1, true),
			2: pluginVersion(testStoragePlugin, 2, true),
		},
	}

	encoded := encodePluginVersions(catalog)
	wantEncoded := "auth=0:2;storage=1:2,1"
	if encoded != wantEncoded {
		t.Fatalf("encoded = %q, want %q", encoded, wantEncoded)
	}

	parsed, err := parsePluginVersionsEnv(encoded)
	if err != nil {
		t.Fatalf("parse error: %s", err)
	}

	negotiated, err := negotiatePlugins(catalog, parsed)
	if err != nil {
		t.Fatalf("negotiate error: %s", err)
	}
	if negotiated[testStoragePlugin] != 2 || negotiated[testAuthPlugin] != 2 {
		t.Fatalf("negotiated = %v, want storage=2 auth=2", negotiated)
	}

	segment := encodeNegotiatedPluginVersions(negotiated)
	wantSegment := "auth=2,storage=2"
	if segment != wantSegment {
		t.Fatalf("segment = %q, want %q", segment, wantSegment)
	}

	reparsed, err := parseNegotiatedPluginVersions(segment)
	if err != nil {
		t.Fatalf("reparse error: %s", err)
	}
	if reparsed[testStoragePlugin] != 2 || reparsed[testAuthPlugin] != 2 {
		t.Fatalf("reparsed = %v", reparsed)
	}
}

// TestPluginVersions_legacyClient verifies that a new server configured with
// PluginVersions produces a predictable global-version result for an old
// client that only speaks the legacy VersionedPlugins negotiation.
func TestPluginVersions_legacyClient(t *testing.T) {
	c := NewClient(&ClientConfig{
		Cmd:             helperProcess("test-service-versions-legacy-client"),
		HandshakeConfig: testVersionedHandshake,
		VersionedPlugins: map[int]PluginSet{
			1: legacyServicePluginMap(),
		},
		StartTimeout: 5 * time.Second,
	})
	defer c.Kill()

	if _, err := c.Start(); err != nil {
		t.Fatalf("Start error: %s", err)
	}

	// Old client receives no per-plugin map and the global version is 1.
	if c.NegotiatedVersion() != 1 {
		t.Fatalf("global version = %d, want 1", c.NegotiatedVersion())
	}
	if versions := c.NegotiatedVersions(); versions != nil {
		t.Fatalf("per-plugin versions should be nil for a legacy client, got %v", versions)
	}

	client, err := c.Client()
	if err != nil {
		t.Fatalf("Client error: %s", err)
	}
	dispenseService(t, client, testStoragePlugin, storageV1Result)
	dispenseService(t, client, testAuthPlugin, authV1Result)
}

// TestPluginVersions_requiredIntersectionError covers the pure negotiation
// matrix without spawning processes.
func TestPluginVersions_requiredIntersectionError(t *testing.T) {
	local := PluginVersions{
		testStoragePlugin: {
			1: pluginVersion(testStoragePlugin, 1, true),
		},
	}

	remote, err := parsePluginVersionsEnv("storage=1:2")
	if err != nil {
		t.Fatalf("parse error: %s", err)
	}

	if _, err := negotiatePlugins(local, remote); err == nil {
		t.Fatal("expected error negotiating required plugin with no common version")
	}

	// Optional local plugin drops instead.
	local[testStoragePlugin][1] = pluginVersion(testStoragePlugin, 1, false)
	negotiated, err := negotiatePlugins(local, remote)
	if err != nil {
		t.Fatalf("optional plugin negotiation error: %s", err)
	}
	if _, ok := negotiated[testStoragePlugin]; ok {
		t.Fatal("optional plugin with no common version should be dropped")
	}

	// Remote-required plugin not offered locally always fails.
	remoteRequired, err := parsePluginVersionsEnv("storage=1:1;auth=1:1")
	if err != nil {
		t.Fatalf("parse error: %s", err)
	}
	if _, err := negotiatePlugins(local, remoteRequired); err == nil {
		t.Fatal("expected error for remote-required auth missing locally")
	}
}

// TestPluginVersions_registeredSetMatchesNegotiation verifies that the
// PluginSet the server registers (and therefore what Dispense reaches) is
// exactly the negotiated name=version pair.
func TestPluginVersions_registeredSetMatchesNegotiation(t *testing.T) {
	catalog := requiredServerServiceVersions()
	remote, err := parsePluginVersionsEnv("storage=1:1;auth=1:1,2")
	if err != nil {
		t.Fatalf("parse error: %s", err)
	}

	negotiated, err := negotiatePlugins(catalog, remote)
	if err != nil {
		t.Fatalf("negotiate error: %s", err)
	}
	if len(negotiated) != 2 || negotiated[testStoragePlugin] != 1 || negotiated[testAuthPlugin] != 2 {
		t.Fatalf("unexpected negotiation result: %v", negotiated)
	}

	set := pluginSetFromNegotiated(catalog, negotiated)
	if len(set) != 2 {
		t.Fatalf("registered set size = %d, want 2: %v", len(set), set)
	}

	storage, ok := set[testStoragePlugin].(*testVersionedServicePlugin)
	if !ok || storage.version != 1 {
		t.Fatalf("registered storage = %#v, want v1", set[testStoragePlugin])
	}
	auth, ok := set[testAuthPlugin].(*testVersionedServicePlugin)
	if !ok || auth.version != 2 {
		t.Fatalf("registered auth = %#v, want v2", set[testAuthPlugin])
	}

	// A stale negotiated version that the catalog does not know must not
	// produce a plugin.
	stale := pluginSetFromNegotiated(catalog, map[string]int{"ghost": 9})
	if len(stale) != 0 {
		t.Fatalf("stale negotiation should register nothing, got %v", stale)
	}
}

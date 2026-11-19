package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const fakeModelsAgy = `#!/bin/sh
echo "$HOME" >> "$AGY_CALL_LOG"
sleep 0.2
case "$HOME" in
*/acct-a) printf 'model-a\tModel A\nshared\tShared\n' ;;
*/acct-b) printf 'model-b\tModel B\nshared\tShared\n' ;;
*/acct-flaky) [ -f "$HOME/ok" ] && printf 'model-flaky\tModel Flaky\n' || exit 1 ;;
*) printf 'model-process\tModel Process\n' ;;
esac
`

type discoveryEnv struct {
	authDir string
	root    string
	callLog string
	cfg     pluginConfig
}

func newDiscoveryEnv(t *testing.T) discoveryEnv {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "process-home"))
	authDir := filepath.Join(root, "auth")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "agy")
	if err := os.WriteFile(binary, []byte(fakeModelsAgy), 0o755); err != nil {
		t.Fatal(err)
	}
	callLog := filepath.Join(root, "calls.log")
	t.Setenv("AGY_CALL_LOG", callLog)
	previousDir := currentAuthDir()
	setAuthDir(authDir)
	invalidateModels()
	t.Cleanup(func() {
		setAuthDir(previousDir)
		invalidateModels()
	})
	cfg := defaultConfig()
	cfg.Mode = modePrint
	cfg.BinaryPath = binary
	setConfig(cfg)
	t.Cleanup(func() { setConfig(defaultConfig()) })
	return discoveryEnv{authDir: authDir, root: root, callLog: callLog, cfg: cfg}
}

func (e discoveryEnv) account(t *testing.T, name string, signedIn bool) string {
	t.Helper()
	home := filepath.Join(e.root, name)
	if err := os.MkdirAll(agyStateDir(home), 0o755); err != nil {
		t.Fatal(err)
	}
	if signedIn {
		if err := os.WriteFile(filepath.Join(agyStateDir(home), "antigravity-oauth-token"), []byte(`{"email":"x"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func (e discoveryEnv) authFile(t *testing.T, file, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.authDir, file), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func modelIDs(models []modelInfo) []string {
	var ids []string
	for _, m := range models {
		if !strings.HasPrefix(m.ID, "agy/default") && !strings.HasPrefix(m.ID, "gemini-3.8") {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

func TestEnabledAgyHomes(t *testing.T) {
	env := newDiscoveryEnv(t)
	a := env.account(t, "acct-a", true)
	b := env.account(t, "acct-b", true)
	notSignedIn := env.account(t, "acct-out", false)
	env.authFile(t, "a.json", `{"type":"agy","home":"`+a+`"}`)
	env.authFile(t, "b-disabled.json", `{"type":"agy","disabled":true,"home":"`+b+`"}`)
	env.authFile(t, "c-out.json", `{"type":"agy","disabled":false,"home":"`+notSignedIn+`"}`)
	env.authFile(t, "d-other.json", `{"type":"codex","home":"`+b+`"}`)
	env.authFile(t, "e-dup.json", `{"type":"agy","home":"`+a+`"}`)
	env.authFile(t, "f-broken.json", `{not json`)
	env.authFile(t, "g-missing.json", `{"type":"agy","home":"`+filepath.Join(env.root, "nowhere")+`"}`)
	env.authFile(t, "h-nohome.json", `{"type":"agy"}`)

	if got, want := enabledAgyHomes(env.authDir), []string{a}; !reflect.DeepEqual(got, want) {
		t.Fatalf("enabledAgyHomes = %v, want %v", got, want)
	}
}

func TestEnabledAgyHomesEmptyCases(t *testing.T) {
	env := newDiscoveryEnv(t)
	if got := enabledAgyHomes(""); got != nil {
		t.Fatalf("empty auth dir gave %v", got)
	}
	if got := enabledAgyHomes(filepath.Join(env.root, "missing")); got != nil {
		t.Fatalf("missing auth dir gave %v", got)
	}
	a := env.account(t, "acct-a", true)
	env.authFile(t, "a.json", `{"type":"agy","disabled":true,"home":"`+a+`"}`)
	if got := enabledAgyHomes(env.authDir); got != nil {
		t.Fatalf("all disabled gave %v", got)
	}
}

func TestEnabledAgyHomesHomelessAuthUsesSignedInProcessHome(t *testing.T) {
	env := newDiscoveryEnv(t)
	env.authFile(t, "nohome.json", `{"type":"agy"}`)
	if got := enabledAgyHomes(env.authDir); got != nil {
		t.Fatalf("signed-out process home gave %v", got)
	}
	processHome := env.account(t, "process-home", true)
	t.Setenv("HOME", processHome)
	if got, want := enabledAgyHomes(env.authDir), []string{""}; !reflect.DeepEqual(got, want) {
		t.Fatalf("enabledAgyHomes = %v, want %v", got, want)
	}
}

func (e discoveryEnv) discoveryCalls(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(e.callLog)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(strings.Fields(string(data)))
}

func forAuth(t *testing.T, authID, home string) ([]string, error) {
	t.Helper()
	request := `{"AuthID":"` + authID + `","AuthProvider":"agy","Metadata":{"home":"` + home + `"}}`
	raw, err := handleMethod(methodModelForAuth, []byte(request))
	if err != nil {
		return nil, err
	}
	var env struct {
		Result struct {
			Provider string
			Models   []modelInfo
		}
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.Result.Provider != providerID {
		t.Fatalf("provider = %q", env.Result.Provider)
	}
	return modelIDs(env.Result.Models), nil
}

func mustForAuth(t *testing.T, authID, home string) []string {
	t.Helper()
	ids, err := forAuth(t, authID, home)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func TestModelForAuthReturnsEachAccountsOwnModels(t *testing.T) {
	env := newDiscoveryEnv(t)
	a := env.account(t, "acct-a", true)
	b := env.account(t, "acct-b", true)
	env.authFile(t, "a.json", `{"type":"agy","home":"`+a+`"}`)
	env.authFile(t, "b.json", `{"type":"agy","home":"`+b+`"}`)

	if got, want := mustForAuth(t, "a", a), []string{"agy/model-a", "agy/shared"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("account a: %v, want %v", got, want)
	}
	if got, want := mustForAuth(t, "b", b), []string{"agy/model-b", "agy/shared"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("account b: %v, want %v", got, want)
	}
	if got, want := mustForAuth(t, "a", a), []string{"agy/model-a", "agy/shared"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("account a again after b: %v, want %v", got, want)
	}
}

func TestModelForAuthFollowsAccountSwitchOfOneAuthID(t *testing.T) {
	env := newDiscoveryEnv(t)
	a := env.account(t, "acct-a", true)
	b := env.account(t, "acct-b", true)
	env.authFile(t, "active.json", `{"type":"agy","home":"`+a+`"}`)
	if got := mustForAuth(t, "active", a); !reflect.DeepEqual(got, []string{"agy/model-a", "agy/shared"}) {
		t.Fatalf("before switch: %v", got)
	}
	env.authFile(t, "active.json", `{"type":"agy","home":"`+b+`"}`)
	if got := mustForAuth(t, "active", b); !reflect.DeepEqual(got, []string{"agy/model-b", "agy/shared"}) {
		t.Fatalf("after switch: %v", got)
	}
}

func TestModelForAuthWithoutHomeUsesProcessHome(t *testing.T) {
	env := newDiscoveryEnv(t)
	env.authFile(t, "legacy.json", `{"type":"agy"}`)
	request := `{"AuthID":"legacy","AuthProvider":"agy","Metadata":{"type":"agy"}}`
	raw, err := handleMethod(methodModelForAuth, []byte(request))
	if err != nil || !strings.Contains(string(raw), "agy/model-process") {
		t.Fatalf("unexpected answer %s, %v", raw, err)
	}
}

func TestModelForAuthRejectsInvalidHome(t *testing.T) {
	env := newDiscoveryEnv(t)
	if _, err := forAuth(t, "x", filepath.Join(env.root, "nowhere")); err == nil {
		t.Fatal("a home that does not exist must fail")
	}
}

func TestModelForAuthDiscoveryFailureIsRetryableAndNotCached(t *testing.T) {
	env := newDiscoveryEnv(t)
	flaky := env.account(t, "acct-flaky", true)
	env.authFile(t, "flaky.json", `{"type":"agy","home":"`+flaky+`"}`)
	previous := catalogRetryAfter
	catalogRetryAfter = 0
	t.Cleanup(func() { catalogRetryAfter = previous })

	_, err := forAuth(t, "flaky", flaky)
	var pe *pluginError
	if !errors.As(err, &pe) || !pe.Retryable {
		t.Fatalf("want a retryable plugin error, got %v", err)
	}
	if got := cachedEnabledModels(env.cfg); !reflect.DeepEqual(modelIDs(got), []string(nil)) {
		t.Fatalf("a failed discovery leaked into the cache: %v", modelIDs(got))
	}

	if err := os.WriteFile(filepath.Join(flaky, "ok"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := mustForAuth(t, "flaky", flaky); !reflect.DeepEqual(got, []string{"agy/model-flaky"}) {
		t.Fatalf("after recovery: %v", got)
	}
}

func TestFailedDiscoveryIsNotRetriedWithinTheBackoff(t *testing.T) {
	env := newDiscoveryEnv(t)
	flaky := env.account(t, "acct-flaky", true)
	env.authFile(t, "flaky.json", `{"type":"agy","home":"`+flaky+`"}`)
	for i := 0; i < 3; i++ {
		if _, err := forAuth(t, "flaky", flaky); err == nil {
			t.Fatal("expected failure")
		}
	}
	if calls := env.discoveryCalls(t); calls != 1 {
		t.Fatalf("agy ran %d times for 3 failing calls inside the backoff", calls)
	}
}

func TestModelStaticAndRegisterNeverRunDiscovery(t *testing.T) {
	env := newDiscoveryEnv(t)
	a := env.account(t, "acct-a", true)
	env.authFile(t, "a.json", `{"type":"agy","home":"`+a+`"}`)
	for _, method := range []string{methodModelStatic, methodModelRegister} {
		raw, err := handleMethod(method, nil)
		if err != nil || !strings.Contains(string(raw), "gemini-3.8-flash") {
			t.Fatalf("%s: %s, %v", method, raw, err)
		}
	}
	if calls := env.discoveryCalls(t); calls != 0 {
		t.Fatalf("model.static/register started discovery %d times", calls)
	}
	mustForAuth(t, "a", a)
	raw, _ := handleMethod(methodModelStatic, nil)
	if !strings.Contains(string(raw), "agy/model-a") {
		t.Fatalf("static does not serve the cached catalog: %s", raw)
	}
}

func TestCountTokensNeverRunsDiscovery(t *testing.T) {
	env := newDiscoveryEnv(t)
	cfg := env.cfg
	cfg.Mode = modeACP
	setConfig(cfg)
	body := []byte(`{"AuthID":"a","Model":"agy/model-a","Payload":"` + base64.StdEncoding.EncodeToString([]byte(`{"messages":[{"role":"user","content":"hi"}]}`)) + `"}`)
	if _, err := handleMethod(methodExecutorCountTokens, body); err != nil {
		t.Fatal(err)
	}
	if calls := env.discoveryCalls(t); calls != 0 {
		t.Fatalf("count_tokens started agy %d times", calls)
	}
}

func TestConcurrentFirstLoadsShareOneDiscovery(t *testing.T) {
	env := newDiscoveryEnv(t)
	a := env.account(t, "acct-a", true)
	cfg := env.cfg
	cfg.AccountHome = a
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := modelIDs(discoverModels(cfg)); !reflect.DeepEqual(got, []string{"agy/model-a", "agy/shared"}) {
				t.Errorf("caller saw %v", got)
			}
		}()
	}
	wg.Wait()
	if calls := env.discoveryCalls(t); calls != 1 {
		t.Fatalf("agy ran %d times for 8 concurrent first loads", calls)
	}
}

func TestRequestTimeHomeDoesNotFlipAccountCatalogs(t *testing.T) {
	env := newDiscoveryEnv(t)
	a := env.account(t, "acct-a", true)
	b := env.account(t, "acct-b", true)
	env.authFile(t, "a.json", `{"type":"agy","home":"`+a+`"}`)
	env.authFile(t, "b.json", `{"type":"agy","home":"`+b+`"}`)
	mustForAuth(t, "a", a)
	mustForAuth(t, "b", b)

	requestCfg := env.cfg
	requestCfg.AccountHome = b
	if native := resolveNativeModel("agy/model-b", requestCfg); native != "model-b" {
		t.Fatalf("resolveNativeModel = %q", native)
	}
	if got := mustForAuth(t, "a", a); !reflect.DeepEqual(got, []string{"agy/model-a", "agy/shared"}) {
		t.Fatalf("account a flipped to %v", got)
	}
}

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

func fakeResident(home string) *acpClient {
	return &acpClient{key: "fake:" + home, home: home, cmd: &exec.Cmd{}, in: nopWriteCloser{}, dead: make(chan struct{})}
}

func TestDisabledAccountIsReclaimedWithoutKillingBusyTurns(t *testing.T) {
	env := newDiscoveryEnv(t)
	a := env.account(t, "acct-a", true)
	b := env.account(t, "acct-b", true)
	env.authFile(t, "a.json", `{"type":"agy","home":"`+a+`"}`)
	env.authFile(t, "b.json", `{"type":"agy","home":"`+b+`"}`)
	mustForAuth(t, "a", a)
	mustForAuth(t, "b", b)
	residentA, residentB := fakeResident(a), fakeResident(b)
	residentB.busy.Store(1)
	acpPool.Lock()
	acpPool.clients = map[string]*acpClient{residentA.key: residentA, residentB.key: residentB}
	acpPool.Unlock()
	t.Cleanup(func() {
		acpPool.Lock()
		acpPool.clients = map[string]*acpClient{}
		acpPool.Unlock()
	})

	env.authFile(t, "b.json", `{"type":"agy","disabled":true,"home":"`+b+`"}`)
	mustForAuth(t, "a", a)

	modelCatalog.Lock()
	for _, entry := range modelCatalog.entries {
		if entry.home == b {
			t.Error("catalog entry of the disabled account survived")
		}
	}
	modelCatalog.Unlock()
	acpPool.Lock()
	_, bPooled := acpPool.clients[residentB.key]
	acpPool.Unlock()
	if bPooled {
		t.Fatal("resident of the disabled account is still pooled")
	}
	time.Sleep(200 * time.Millisecond)
	if !residentB.alive() {
		t.Fatal("resident was killed under a running turn")
	}
	if !residentA.alive() {
		t.Fatal("resident of the enabled account was closed")
	}
	residentB.busy.Store(0)
	deadline := time.Now().Add(5 * time.Second)
	for residentB.alive() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if residentB.alive() {
		t.Fatal("idle resident of the disabled account was never closed")
	}
}

func TestRegistrationUsesPerAuthModels(t *testing.T) {
	caps := pluginRegistration().Capabilities
	if caps.ExecutorModelScope != "oauth" {
		t.Fatalf("ExecutorModelScope = %q; the host only calls model.for_auth under oauth", caps.ExecutorModelScope)
	}
	if caps.ModelRouter {
		t.Fatal("the host rejects self routes under oauth, so no model router may be declared")
	}
}

func TestModelRouteAnswersNotHandled(t *testing.T) {
	raw, err := handleMethod(methodModelRoute, []byte(`{"RequestedModel":"agy/x"}`))
	if err != nil || strings.Contains(string(raw), `"Handled":true`) {
		t.Fatalf("unexpected route answer %s, %v", raw, err)
	}
}

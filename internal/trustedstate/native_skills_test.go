package trustedstate

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func skillFiles(body string, extra ...NativeFile) []NativeFile {
	files := []NativeFile{{Path: "SKILL.md", Content: []byte(body)}}
	return append(files, extra...)
}

func validSkillBody(name string) string {
	return "---\nname: " + name + "\ndescription: A tested skill package.\n---\n\nRead [details](references/details.md).\n"
}

func openSkillsTest(t *testing.T) *Store {
	t.Helper()
	store, _ := openTest(t)
	return store
}

func TestSkillPackageLifecycleBindsAuthorityAndSnapshot(t *testing.T) {
	s := openSkillsTest(t)
	files := skillFiles(validSkillBody("demo"), NativeFile{Path: "references/details.md", Content: []byte("v1")})
	installed, err := s.SkillPackageReplace("skill-install", "host", "demo", 0, true, files)
	if err != nil || installed.Error != "" || installed.Resources[0].Revision != 1 {
		t.Fatal(installed, err)
	}
	listed, err := s.SkillPackageList("host")
	if err != nil || len(listed.Packages) != 1 || listed.Packages[0].Authority != "host" || listed.Packages[0].Files[0].SHA256 == "" {
		t.Fatal(listed, err)
	}
	read, err := s.SkillPackageRead("host", "demo", 1, "references/details.md")
	if err != nil || string(read.Content) != "v1" {
		t.Fatal(read, err)
	}
	if _, err = s.SkillPackageRead("executor", "demo", 1, "SKILL.md"); err == nil || err.Error() != "authority_mismatch" {
		t.Fatal("authority escalation was accepted", err)
	}
	if _, err = s.SkillPackageReplace("skill-takeover", "executor", "demo", 1, true, files); err == nil || err.Error() != "authority_mismatch" {
		t.Fatal("authority takeover was accepted", err)
	}
	files[1].Content = []byte("v2")
	upgraded, err := s.SkillPackageReplace("skill-upgrade", "host", "demo", 1, true, files)
	if err != nil || upgraded.Error != "" || upgraded.Resources[0].Revision != 2 {
		t.Fatal(upgraded, err)
	}
	if _, err = s.SkillPackageRead("host", "demo", 1, "SKILL.md"); err == nil || err.Error() != "snapshot_expired" {
		t.Fatal("old snapshot silently read upgraded package", err)
	}
	disabled, err := s.SkillPackageSetEnabled("skill-disable", "host", "demo", 2, false)
	retry, retryErr := s.SkillPackageSetEnabled("skill-disable", "host", "demo", 2, false)
	if err != nil || retryErr != nil || disabled.Error != "" || disabled.CommitSequence != retry.CommitSequence || disabled.Resources[0].Package.Enabled {
		t.Fatal(disabled, retry, err, retryErr)
	}
	if _, err = s.SkillPackageSetEnabled("skill-disable", "host", "demo", 2, true); err == nil {
		t.Fatal("request id accepted changed enable intent")
	}
	deleted, err := s.SkillPackageDelete("skill-delete", "host", "demo", 3)
	retry, retryErr = s.SkillPackageDelete("skill-delete", "host", "demo", 3)
	if err != nil || retryErr != nil || deleted.Error != "" || deleted.CommitSequence != retry.CommitSequence {
		t.Fatal(deleted, retry, err, retryErr)
	}
	listed, _ = s.SkillPackageList("host")
	if len(listed.Packages) != 0 {
		t.Fatal("deleted package remained active", listed)
	}
}

func TestSkillPackageValidationAndLegacyMigration(t *testing.T) {
	s := openSkillsTest(t)
	bad := []struct {
		id    string
		files []NativeFile
	}{
		{"missing-frontmatter", skillFiles("plain markdown")},
		{"wrong-name", skillFiles(validSkillBody("different"), NativeFile{Path: "references/details.md", Content: []byte("x")})},
		{"missing-asset", skillFiles(validSkillBody("missing-asset"))},
		{"missing-outside-fence", skillFiles("---\nname: missing-outside-fence\ndescription: Missing dependency.\n---\n[missing](missing.md)\n")},
	}
	fenced := skillFiles("---\nname: fenced-example\ndescription: Example links are not dependencies.\n---\n```markdown\n[example](not-packaged.md)\n```\n")
	if result, err := s.SkillPackageReplace("fenced-example", "host", "fenced-example", 0, true, fenced); err != nil || result.Error != "" {
		t.Fatal("fenced example link was treated as a package dependency", result, err)
	}
	for i, tc := range bad {
		if _, err := s.SkillPackageReplace("bad-skill-"+jsonNumber(uint64(i)), "host", tc.id, 0, true, tc.files); err == nil {
			t.Fatalf("invalid package %q accepted", tc.id)
		}
	}
	invalid := mutation(t, s, "skills", "put", Request{ID: "legacy-invalid", Content: "plain", RequestID: "legacy-invalid-put"})
	if _, err := s.ImportLegacySkill("legacy-invalid-import", "legacy-invalid", "host", invalid.Object.Revision); err == nil {
		t.Fatal("invalid v1 skill imported")
	}
	if result := mutation(t, s, "skills", "put", Request{ID: "legacy-invalid", Content: "still writable", ExpectedRevision: invalid.Object.Revision, RequestID: "legacy-invalid-update"}); result.Error != "" {
		t.Fatal("failed migration froze source", result)
	}
	body := "---\nname: legacy\ndescription: Legacy skill.\n---\nBody\n"
	legacy := mutation(t, s, "skills", "put", Request{ID: "legacy", Content: body, RequestID: "legacy-put"})
	first, err := s.ImportLegacySkill("legacy-import", "legacy", "host", legacy.Object.Revision)
	second, retryErr := s.ImportLegacySkill("legacy-import", "legacy", "host", legacy.Object.Revision)
	if err != nil || retryErr != nil || first.CommitSequence != second.CommitSequence {
		t.Fatal(first, second, err, retryErr)
	}
	if result := mutation(t, s, "skills", "put", Request{ID: "legacy", Content: body, ExpectedRevision: legacy.Object.Revision, RequestID: "legacy-after"}); result.Error != "migrated_read_only" {
		t.Fatal("migrated source remained writable", result)
	}
}

func TestBundledSkillsEnsureIsAtomicAndReplayable(t *testing.T) {
	s, root := openTest(t)
	packages := []BundledSkillPackage{
		{PackageID: "alpha", Files: skillFiles("---\nname: alpha\ndescription: Alpha.\n---\nA\n")},
		{PackageID: "beta", Files: skillFiles("---\nname: beta\ndescription: Beta.\n---\nB\n")},
	}
	first, err := s.SkillBundledEnsure("bundled-v1", "host", true, packages)
	replay, replayErr := s.SkillBundledEnsure("bundled-v1", "host", true, packages)
	if err != nil || replayErr != nil || first.Error != "" || first.CommitSequence != replay.CommitSequence || len(first.Resources) != 3 {
		t.Fatal(first, replay, err, replayErr)
	}
	s.Close()
	s, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	replay, replayErr = s.SkillBundledEnsure("bundled-v1", "host", true, packages)
	if replayErr != nil || replay.CommitSequence != first.CommitSequence || len(replay.Resources) != 3 {
		t.Fatal("stable bundled request did not replay after restart", replay, replayErr)
	}
	journal, err := os.ReadFile(filepath.Join(root, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if count := bytes.Count(bytes.TrimSpace(journal), []byte("\n")) + 1; count != 1 {
		t.Fatalf("bundled retry appended audit records: got %d", count)
	}
	disabled, err := s.SkillPackageSetEnabled("disable-alpha", "host", "alpha", 1, false)
	if err != nil || disabled.Error != "" {
		t.Fatal(disabled, err)
	}
	packages[0].Files[0].Content = []byte("---\nname: alpha\ndescription: Alpha v2.\n---\nA2\n")
	packages = packages[:1]
	upgraded, err := s.SkillBundledEnsure("bundled-v2", "host", true, packages)
	if err != nil || upgraded.Error != "" || len(upgraded.Resources) != 3 {
		t.Fatal(upgraded, err)
	}
	listed, err := s.SkillPackageList("host")
	if err != nil || len(listed.Packages) != 1 || listed.Packages[0].PackageID != "alpha" || listed.Packages[0].Enabled || listed.Packages[0].Revision != 3 {
		t.Fatal(listed, err)
	}
	if _, err = s.SkillPackageRead("host", "beta", 1, "SKILL.md"); err == nil || err.Error() != "snapshot_expired" {
		t.Fatal("removed bundled skill remained readable", err)
	}
	reintroduced := []BundledSkillPackage{{PackageID: "beta", Files: skillFiles("---\nname: beta\ndescription: Beta returns.\n---\nB2\n")}}
	result, err := s.SkillBundledEnsure("bundled-v3", "host", true, reintroduced)
	if err != nil || result.Error != "" {
		t.Fatal("managed tombstone could not be reintroduced", result, err)
	}
}

func TestBundledSkillsEnsureRejectsUnmanagedCollision(t *testing.T) {
	s := openSkillsTest(t)
	files := skillFiles("---\nname: demo\ndescription: User package.\n---\nBody\n")
	if result, err := s.SkillPackageReplace("user-install", "host", "demo", 0, true, files); err != nil || result.Error != "" {
		t.Fatal(result, err)
	}
	if _, err := s.SkillBundledEnsure("bundled-collision", "host", true, []BundledSkillPackage{{PackageID: "demo", Files: files}}); err == nil || err.Error() != "bundled skill package collision" {
		t.Fatal("unmanaged package was overwritten", err)
	}
}

func TestNativeSkillsHTTPIsInstallerOnlyAndAuthorityBound(t *testing.T) {
	s := openSkillsTest(t)
	installerToken := strings.Repeat("i", 64)
	modelToken := strings.Repeat("m", 64)
	handler, err := NativeHTTPHandler(s, []NativeCredential{{Token: installerToken, Kind: "installer", ThreadID: "host"}, {Token: modelToken, Kind: "model_tool", ThreadID: "thread"}})
	if err != nil {
		t.Fatal(err)
	}
	request := func(token, operation string, value any) *httptest.ResponseRecorder {
		body, _ := json.Marshal(value)
		req := httptest.NewRequest(http.MethodPost, "/native/v2/"+operation, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, req)
		return out
	}
	handshake := request(installerToken, "handshake", struct{}{})
	if handshake.Code != 200 || !strings.Contains(handshake.Body.String(), "native_skills_packages") {
		t.Fatal(handshake)
	}
	files := skillFiles("---\nname: demo\ndescription: Demo.\n---\nBody\n")
	writableFiles := []map[string]any{{"path": files[0].Path, "content_base64": files[0].Content}}
	payload := map[string]any{"request_id": "http-install", "package_id": "demo", "expected_revision": 0, "enabled": true, "files": writableFiles}
	if response := request(modelToken, "skills.package.replace", payload); response.Code != 403 {
		t.Fatal("model credential installed package", response)
	}
	if response := request(installerToken, "skills.package.replace", payload); response.Code != 200 || response.Header().Get("X-RCA-Commit-Sequence") == "" {
		t.Fatal(response)
	}
	list := request(installerToken, "skills.package.list", struct{}{})
	if list.Code != 200 || !strings.Contains(list.Body.String(), `"authority":"host"`) {
		t.Fatal(list)
	}
	read := request(installerToken, "skills.package.read", map[string]any{"package_id": "demo", "revision": 1, "path": "SKILL.md"})
	if read.Code != 200 || !strings.Contains(read.Body.String(), `"package_id":"demo"`) {
		t.Fatal(read)
	}
}

package trustedstate

import (
	"bytes"
	"encoding/json"
	"errors"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

var markdownLink = regexp.MustCompile(`\]\(([^)]+)\)`)

type SkillPackageFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type SkillPackageSummary struct {
	PackageID string             `json:"package_id"`
	Revision  uint64             `json:"revision"`
	Authority string             `json:"authority"`
	Enabled   bool               `json:"enabled"`
	Files     []SkillPackageFile `json:"files"`
}

type SkillPackageList struct {
	Snapshot uint64                `json:"snapshot"`
	Packages []SkillPackageSummary `json:"packages"`
}

type SkillPackageRead struct {
	PackageID string `json:"package_id"`
	Revision  uint64 `json:"revision"`
	Authority string `json:"authority"`
	Path      string `json:"path"`
	Content   []byte `json:"content_base64"`
	SHA256    string `json:"sha256"`
}

type BundledSkillPackage struct {
	PackageID string       `json:"package_id"`
	Files     []NativeFile `json:"files"`
}

type bundledSkillManifest struct {
	Packages []string `json:"packages"`
	Managed  []string `json:"managed"`
}

const bundledSkillManifestKey = "skills/bundled.json"

func cloneSkillResource(resource NativeResource) NativeResource {
	data, _ := json.Marshal(resource)
	var cloned NativeResource
	_ = json.Unmarshal(data, &cloned)
	return cloned
}

func validateSkillPackage(packageID string, p *NativePackage) error {
	if p == nil || !logicalID.MatchString(packageID) || !logicalID.MatchString(p.Authority) {
		return errors.New("invalid skill package identity")
	}
	files := make(map[string][]byte, len(p.Files))
	for _, file := range p.Files {
		files[file.Path] = file.Content
	}
	main, ok := files["SKILL.md"]
	if !ok || !utf8.Valid(main) {
		return errors.New("invalid SKILL.md")
	}
	text := string(main)
	if !strings.HasPrefix(text, "---\n") {
		return errors.New("invalid SKILL.md frontmatter")
	}
	end := strings.Index(text[4:], "\n---\n")
	if end < 0 {
		return errors.New("invalid SKILL.md frontmatter")
	}
	frontmatter := text[4 : 4+end]
	name := ""
	description := ""
	for _, line := range strings.Split(frontmatter, "\n") {
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		switch strings.TrimSpace(key) {
		case "name":
			name = strings.Trim(strings.TrimSpace(value), `"'`)
		case "description":
			description = strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	if name != packageID || description == "" || len(description) > 4096 {
		return errors.New("invalid SKILL.md name or description")
	}
	for _, match := range markdownLink.FindAllStringSubmatch(markdownWithoutFencedCode(text), -1) {
		target := strings.TrimSpace(strings.SplitN(match[1], "#", 2)[0])
		if target == "" || strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
			continue
		}
		clean := path.Clean(target)
		if !validNativePath(clean) {
			return errors.New("invalid SKILL.md resource reference")
		}
		if _, exists := files[clean]; !exists {
			return errors.New("missing SKILL.md resource reference")
		}
	}
	return nil
}

func markdownWithoutFencedCode(text string) string {
	var visible strings.Builder
	fence := ""
	for _, line := range strings.SplitAfter(text, "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		if fence == "" && (strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")) {
			fence = trimmed[:3]
			continue
		}
		if fence != "" {
			if strings.HasPrefix(trimmed, fence) {
				fence = ""
			}
			continue
		}
		visible.WriteString(line)
	}
	return visible.String()
}

func (s *Store) SkillPackageReplace(requestID, authority, packageID string, expectedRevision uint64, enabled bool, files []NativeFile) (NativeResult, error) {
	intent := nativeIntent([]any{authority, packageID, expectedRevision, enabled, files})
	if result, found, err := s.skillRequestReplay(requestID, "skills.package.replace", intent); found || err != nil {
		return result, err
	}
	if expectedRevision != 0 {
		current, err := s.NativeRead("skills.package", packageID, expectedRevision)
		if err != nil {
			return NativeResult{}, err
		}
		if current.Deleted || current.Package == nil || current.Package.Authority != authority {
			return NativeResult{}, errors.New("authority_mismatch")
		}
	}
	change := NativeChange{Domain: "skills.package", Key: packageID, ExpectedRevision: expectedRevision, Package: &NativePackage{Authority: authority, Enabled: enabled, Files: files}}
	return s.NativeBatch(NativeRequest{RequestID: requestID, Actor: NativeActor{Kind: "installer", ThreadID: authority}, Operation: "skills.package.replace", Intent: intent, Changes: []NativeChange{change}})
}

func (s *Store) SkillBundledEnsure(requestID, authority string, enabled bool, packages []BundledSkillPackage) (NativeResult, error) {
	intent := nativeIntent([]any{authority, enabled, packages})
	if result, found, err := s.skillRequestReplay(requestID, "skills.bundled.ensure", intent); found || err != nil {
		return result, err
	}
	sorted := append([]BundledSkillPackage(nil), packages...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].PackageID < sorted[j].PackageID })
	desired := make(map[string]BundledSkillPackage, len(sorted))
	for _, bundled := range sorted {
		if _, duplicate := desired[bundled.PackageID]; duplicate {
			return NativeResult{}, errors.New("duplicate bundled skill package")
		}
		packageValue := &NativePackage{Authority: authority, Enabled: enabled, Files: bundled.Files}
		if err := validateSkillPackage(bundled.PackageID, packageValue); err != nil {
			return NativeResult{}, err
		}
		desired[bundled.PackageID] = bundled
	}

	s.mu.Lock()
	marker := cloneSkillResource(s.native[nativeKey("maintenance", bundledSkillManifestKey)])
	current := make(map[string]NativeResource, len(desired))
	for packageID := range desired {
		current[packageID] = cloneSkillResource(s.native[nativeKey("skills.package", packageID)])
	}
	var previous bundledSkillManifest
	if marker.Revision != 0 && !marker.Deleted {
		if err := strictJSON(marker.Content, &previous); err != nil {
			s.mu.Unlock()
			return NativeResult{}, errors.New("invalid bundled skill manifest")
		}
		for _, packageID := range previous.Packages {
			if _, ok := current[packageID]; !ok {
				current[packageID] = cloneSkillResource(s.native[nativeKey("skills.package", packageID)])
			}
		}
	}
	s.mu.Unlock()

	managedIDs := append(append([]string(nil), previous.Managed...), previous.Packages...)
	managed := make(map[string]bool, len(managedIDs))
	for _, packageID := range managedIDs {
		if !logicalID.MatchString(packageID) {
			return NativeResult{}, errors.New("invalid bundled skill manifest")
		}
		managed[packageID] = true
	}
	changes := make([]NativeChange, 0, len(sorted)+len(previous.Packages)+1)
	packageIDs := make([]string, 0, len(sorted))
	for _, bundled := range sorted {
		packageIDs = append(packageIDs, bundled.PackageID)
		old := current[bundled.PackageID]
		if old.Revision != 0 && !managed[bundled.PackageID] {
			return NativeResult{}, errors.New("bundled skill package collision")
		}
		active := enabled
		if old.Revision != 0 && !old.Deleted {
			if old.Package == nil || old.Package.Authority != authority {
				return NativeResult{}, errors.New("authority_mismatch")
			}
			active = old.Package.Enabled
			if nativeFilesEqual(old.Package.Files, bundled.Files) {
				continue
			}
		}
		managed[bundled.PackageID] = true
		changes = append(changes, NativeChange{Domain: "skills.package", Key: bundled.PackageID, ExpectedRevision: old.Revision, Package: &NativePackage{Authority: authority, Enabled: active, Files: bundled.Files}})
	}
	for _, packageID := range previous.Packages {
		if _, retained := desired[packageID]; retained {
			continue
		}
		old := current[packageID]
		if old.Revision == 0 || old.Deleted || old.Package == nil || old.Package.Authority != authority {
			return NativeResult{}, errors.New("authority_mismatch")
		}
		changes = append(changes, NativeChange{Domain: "skills.package", Key: packageID, ExpectedRevision: old.Revision, Deleted: true})
	}
	allManaged := make([]string, 0, len(managed))
	for packageID := range managed {
		allManaged = append(allManaged, packageID)
	}
	sort.Strings(allManaged)
	manifestContent, _ := json.Marshal(bundledSkillManifest{Packages: packageIDs, Managed: allManaged})
	changes = append(changes, NativeChange{Domain: "maintenance", Key: bundledSkillManifestKey, ExpectedRevision: marker.Revision, Content: manifestContent})
	return s.NativeBatch(NativeRequest{RequestID: requestID, Actor: NativeActor{Kind: "maintenance", ThreadID: authority}, Operation: "skills.bundled.ensure", Intent: intent, Changes: changes})
}

func nativeFilesEqual(a, b []NativeFile) bool {
	if len(a) != len(b) {
		return false
	}
	left := append([]NativeFile(nil), a...)
	right := append([]NativeFile(nil), b...)
	sort.Slice(left, func(i, j int) bool { return left[i].Path < left[j].Path })
	sort.Slice(right, func(i, j int) bool { return right[i].Path < right[j].Path })
	for i := range left {
		if left[i].Path != right[i].Path || !bytes.Equal(left[i].Content, right[i].Content) {
			return false
		}
	}
	return true
}

func (s *Store) SkillPackageSetEnabled(requestID, authority, packageID string, expectedRevision uint64, enabled bool) (NativeResult, error) {
	intent := nativeIntent([]any{authority, packageID, expectedRevision, enabled})
	if result, found, err := s.skillRequestReplay(requestID, "skills.package.enable", intent); found || err != nil {
		return result, err
	}
	current, err := s.NativeRead("skills.package", packageID, expectedRevision)
	if err != nil {
		return NativeResult{}, err
	}
	if current.Deleted || current.Package == nil || current.Package.Authority != authority {
		return NativeResult{}, errors.New("authority_mismatch")
	}
	change := NativeChange{Domain: "skills.package", Key: packageID, ExpectedRevision: expectedRevision, Package: &NativePackage{Authority: authority, Enabled: enabled, Files: current.Package.Files}}
	return s.NativeBatch(NativeRequest{RequestID: requestID, Actor: NativeActor{Kind: "installer", ThreadID: authority}, Operation: "skills.package.enable", Intent: intent, Changes: []NativeChange{change}})
}

func (s *Store) SkillPackageDelete(requestID, authority, packageID string, expectedRevision uint64) (NativeResult, error) {
	intent := nativeIntent([]any{authority, packageID, expectedRevision})
	if result, found, err := s.skillRequestReplay(requestID, "skills.package.delete", intent); found || err != nil {
		return result, err
	}
	current, err := s.NativeRead("skills.package", packageID, expectedRevision)
	if err != nil {
		return NativeResult{}, err
	}
	if current.Deleted || current.Package == nil || current.Package.Authority != authority {
		return NativeResult{}, errors.New("authority_mismatch")
	}
	change := NativeChange{Domain: "skills.package", Key: packageID, ExpectedRevision: expectedRevision, Deleted: true}
	return s.NativeBatch(NativeRequest{RequestID: requestID, Actor: NativeActor{Kind: "installer", ThreadID: authority}, Operation: "skills.package.delete", Intent: intent, Changes: []NativeChange{change}})
}

func (s *Store) skillRequestReplay(requestID, operation, intent string) (NativeResult, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if record, ok := s.nativeRequests[requestID]; ok {
		if record.Request.Operation != operation || record.Request.Intent != intent {
			return NativeResult{}, true, errors.New("request_id reused with different arguments")
		}
		return cloneNativeResult(record.Result), true, nil
	}
	return NativeResult{}, false, nil
}

func (s *Store) SkillPackageList(authority string) (SkillPackageList, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poisoned {
		return SkillPackageList{}, errors.New("state unavailable")
	}
	out := SkillPackageList{Snapshot: s.sequence, Packages: []SkillPackageSummary{}}
	for _, resource := range s.native {
		if resource.Domain != "skills.package" || resource.Deleted || resource.Package == nil || resource.Package.Authority != authority {
			continue
		}
		summary := SkillPackageSummary{PackageID: resource.Key, Revision: resource.Revision, Authority: resource.Package.Authority, Enabled: resource.Package.Enabled}
		for _, file := range resource.Package.Files {
			summary.Files = append(summary.Files, SkillPackageFile{Path: file.Path, SHA256: file.SHA256})
		}
		sort.Slice(summary.Files, func(i, j int) bool { return summary.Files[i].Path < summary.Files[j].Path })
		out.Packages = append(out.Packages, summary)
	}
	sort.Slice(out.Packages, func(i, j int) bool { return out.Packages[i].PackageID < out.Packages[j].PackageID })
	return out, nil
}

func (s *Store) SkillPackageRead(authority, packageID string, revision uint64, filePath string) (SkillPackageRead, error) {
	resource, err := s.NativeRead("skills.package", packageID, revision)
	if err != nil {
		return SkillPackageRead{}, err
	}
	if resource.Deleted || resource.Package == nil || resource.Package.Authority != authority {
		return SkillPackageRead{}, errors.New("authority_mismatch")
	}
	for _, file := range resource.Package.Files {
		if file.Path == filePath {
			return SkillPackageRead{PackageID: packageID, Revision: resource.Revision, Authority: authority, Path: filePath, Content: append([]byte(nil), file.Content...), SHA256: file.SHA256}, nil
		}
	}
	return SkillPackageRead{}, errors.New("not_found")
}

func (s *Store) ImportLegacySkill(requestID, id, authority string, revision uint64) (NativeResult, error) {
	old, err := s.Read("skills", id)
	if err != nil {
		return NativeResult{}, err
	}
	if old.Revision != revision || old.Deleted {
		return NativeResult{}, errors.New("source_revision_conflict")
	}
	packageValue := &NativePackage{Authority: authority, Enabled: true, Files: []NativeFile{{Path: "SKILL.md", Content: []byte(old.Content)}}}
	if err = validateSkillPackage(id, packageValue); err != nil {
		return NativeResult{}, err
	}
	alias := LegacyAlias{Collection: "skills", ID: id, Revision: revision, SourceHash: hashBytes([]byte(old.Content)), Domain: "skills.package", Key: id}
	encoded, _ := json.Marshal(alias)
	return s.NativeBatch(NativeRequest{RequestID: requestID, Actor: NativeActor{Kind: "migration"}, Operation: "migration.import_v1", Changes: []NativeChange{{Domain: "skills.package", Key: id, Package: packageValue}, {Domain: "migration", Key: "legacy/skills/" + id, Content: encoded}}})
}

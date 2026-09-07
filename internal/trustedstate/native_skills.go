package trustedstate

import (
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
	for _, match := range markdownLink.FindAllStringSubmatch(text, -1) {
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

func (s *Store) SkillPackageReplace(requestID, authority, packageID string, expectedRevision uint64, enabled bool, files []NativeFile) (NativeResult, error) {
	change := NativeChange{Domain: "skills.package", Key: packageID, ExpectedRevision: expectedRevision, Package: &NativePackage{Authority: authority, Enabled: enabled, Files: files}}
	return s.NativeBatch(NativeRequest{RequestID: requestID, Actor: NativeActor{Kind: "installer", ThreadID: authority}, Operation: "skills.package.replace", Intent: nativeIntent([]any{authority, packageID, expectedRevision, enabled, files}), Changes: []NativeChange{change}})
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

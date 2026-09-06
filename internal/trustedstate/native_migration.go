package trustedstate

import (
	"encoding/json"
	"errors"
)

// LegacyAlias freezes the old object in the same commit that publishes its
// canonical native replacement. It never makes a v1 string an active skill.
type LegacyAlias struct {
	Collection string `json:"collection"`
	ID         string `json:"id"`
	Revision   uint64 `json:"revision"`
	SourceHash string `json:"source_hash"`
	Domain     string `json:"domain"`
	Key        string `json:"key"`
}

func (s *Store) ImportLegacyMemory(requestID, id, filename string, revision uint64) (NativeResult, error) {
	if !noteFilename.MatchString(filename) {
		return NativeResult{}, errors.New("invalid_filename")
	}
	old, e := s.Read("memory", id)
	if e != nil {
		return NativeResult{}, e
	}
	if old.Revision != revision || old.Deleted {
		return NativeResult{}, errors.New("source_revision_conflict")
	}
	a := LegacyAlias{Collection: "memory", ID: id, Revision: revision, SourceHash: hashBytes([]byte(old.Content)), Domain: "memory.note", Key: filename}
	b, _ := json.Marshal(a)
	return s.NativeBatch(NativeRequest{RequestID: requestID, Actor: NativeActor{Kind: "migration"}, Operation: "migration.import_v1", Changes: []NativeChange{{Domain: a.Domain, Key: a.Key, Content: []byte(old.Content)}, {Domain: "migration", Key: "legacy/memory/" + id, Content: b}}})
}
func (s *Store) validateLegacyAlias(q NativeRequest) string {
	for _, c := range q.Changes {
		if c.Domain != "migration" {
			continue
		}
		if q.Operation != "migration.import_v1" || q.Actor.Kind != "migration" || c.Deleted {
			return "invalid_migration"
		}
		var a LegacyAlias
		if strictJSON(c.Content, &a) != nil || a.Collection != "memory" || !logicalID.MatchString(a.ID) || a.Domain != "memory.note" || !noteFilename.MatchString(a.Key) || c.Key != "legacy/memory/"+a.ID {
			return "invalid_migration"
		}
		old := s.objects[a.Collection+"/"+a.ID]
		if old.Revision != a.Revision || old.Deleted || a.Revision == 0 || hashBytes([]byte(old.Content)) != a.SourceHash {
			return "source_revision_conflict"
		}
		found := false
		for _, target := range q.Changes {
			if target.Domain == a.Domain && target.Key == a.Key && !target.Deleted && string(target.Content) == old.Content {
				found = true
			}
		}
		if !found {
			return "missing_migration_target"
		}
	}
	return ""
}

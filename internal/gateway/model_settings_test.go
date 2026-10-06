package gateway

import "testing"

func boolPtr(v bool) *bool { return &v }
func TestModelPolicyChanges(t *testing.T) {
	policy := modelPolicy{Excluded: map[string][]string{"claude": {"old"}, "other": {"keep"}}, Aliases: map[string][]modelAlias{"other": {{Name: "original", Alias: "untouched", Fork: true}}}}
	settings := modelSettings{Models: []managedModel{
		{Model: Model{ID: "old", Provider: "Claude"}},
		{Model: Model{ID: "new", Provider: "Claude"}, Enabled: true},
		{Model: Model{ID: "gpt", Provider: "Codex"}, Enabled: true},
	}, Revision: policyRevision(policy)}
	result, patch, err := applyModelChanges(settings, policy, []modelChange{{ID: "old", Provider: "Claude", Enabled: boolPtr(true), Alias: "fast"}, {ID: "new", Provider: "Claude", Enabled: boolPtr(false)}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Models[0].Alias != "fast" || !result.Models[0].Enabled || result.Models[1].Enabled {
		t.Fatalf("wrong changes: %+v", result)
	}
	if len(patch.Excluded) != 1 || len(patch.Aliases) != 1 || patch.Excluded["claude"][0] != "new" || patch.Aliases["claude"][0].Fork {
		t.Fatalf("patch changes unrelated providers or alias semantics: %+v", patch)
	}
	if policy.Excluded["claude"][0] != "old" || len(policy.Aliases["claude"]) != 0 {
		t.Fatal("mutated original snapshot")
	}
	for _, tc := range []struct {
		name    string
		changes []modelChange
	}{
		{"native collision", []modelChange{{ID: "old", Provider: "Claude", Enabled: boolPtr(true), Alias: "gpt"}}},
		{"alias collision", []modelChange{{ID: "old", Provider: "Claude", Enabled: boolPtr(true), Alias: "fast"}, {ID: "new", Provider: "Claude", Enabled: boolPtr(true), Alias: "FAST"}}},
		{"unsafe alias", []modelChange{{ID: "old", Provider: "Claude", Enabled: boolPtr(true), Alias: "../bad"}}},
		{"unknown", []modelChange{{ID: "missing", Provider: "Claude", Enabled: boolPtr(true)}}},
		{"missing enabled", []modelChange{{ID: "old", Provider: "Claude"}}},
		{"duplicate", []modelChange{{ID: "old", Provider: "Claude", Enabled: boolPtr(true)}, {ID: "old", Provider: "Claude", Enabled: boolPtr(false)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := applyModelChanges(settings, policy, tc.changes); err == nil {
				t.Fatal("accepted invalid changes")
			}
		})
	}
}

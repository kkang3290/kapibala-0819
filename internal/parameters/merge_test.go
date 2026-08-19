package parameters

import (
	"reflect"
	"testing"
)

func TestParameterEvolution(t *testing.T) {
	base := Values{"sender": "base", "region": "cn", "retries": float64(1)}
	group := Values{"sender": "", "region": "east", "group_only": true}

	current := AtTaskStart(base, group)
	assertValues(t, current, Values{
		"sender": "", "region": "east", "retries": float64(1), "group_only": true,
	})

	current = AtStep(current, Values{"sender": "step-1", "region": "", "new_key": "first"})
	assertValues(t, current, Values{
		"sender": "step-1", "region": "east", "retries": float64(1), "group_only": true, "new_key": "first",
	})

	current = AtStep(current, Values{"sender": "", "new_key": ""})
	assertValues(t, current, Values{
		"sender": "step-1", "region": "east", "retries": float64(1), "group_only": true, "new_key": "first",
	})

	current = AtStep(current, Values{"sender": "step-3", "missing": "", "retries": float64(0)})
	assertValues(t, current, Values{
		"sender": "step-3", "region": "east", "retries": float64(0), "group_only": true, "new_key": "first",
	})
}

func TestMergesDoNotMutateInputs(t *testing.T) {
	base := Values{"a": "base"}
	group := Values{"a": "group"}
	started := AtTaskStart(base, group)
	stepped := AtStep(started, Values{"a": "step"})

	assertValues(t, base, Values{"a": "base"})
	assertValues(t, group, Values{"a": "group"})
	assertValues(t, started, Values{"a": "group"})
	assertValues(t, stepped, Values{"a": "step"})
}

func TestStepEmptyStringDoesNotCreateMissingKey(t *testing.T) {
	got := AtStep(Values{"existing": "value"}, Values{"new": ""})
	assertValues(t, got, Values{"existing": "value"})
}

func assertValues(t *testing.T, got, want Values) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("values mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

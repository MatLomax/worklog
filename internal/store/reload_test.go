package store

import (
	"reflect"
	"testing"
)

// TestSetConfigSwapsStatuses checks that SetConfig replaces every derived use
// of the status configuration: settable names, the default, and the SQL-side
// closed list; that it copies its argument; and that an invalid config is
// rejected without disturbing the one in force.
func TestSetConfigSwapsStatuses(t *testing.T) {
	st, err := OpenWithConfig(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.CreateTask(CreateTaskInput{Title: "Old", Status: "pending"}); err != nil {
		t.Fatalf("create pending under defaults: %v", err)
	}

	cfg := &Config{Statuses: []StatusDef{
		{Name: "todo", Kind: KindOpen, Default: true},
		{Name: "fin", Kind: KindClosed},
	}}
	if err := st.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	cfg.Statuses[0].Name = "mutated" // the store must hold its own copy
	if got := st.Config().Names(); !reflect.DeepEqual(got, []string{"todo", "fin"}) {
		t.Fatalf("Config() after SetConfig = %v", got)
	}

	if _, err := st.CreateTask(CreateTaskInput{Title: "Rejected", Status: "pending"}); err == nil {
		t.Fatal("create with pending succeeded after it was dropped from the config")
	}
	tk, err := st.CreateTask(CreateTaskInput{Title: "Fresh"})
	if err != nil {
		t.Fatal(err)
	}
	if tk.Status != "todo" {
		t.Fatalf("default status after SetConfig = %q, want todo", tk.Status)
	}
	if _, err := st.CreateTask(CreateTaskInput{Title: "Finished", Status: "fin"}); err != nil {
		t.Fatal(err)
	}
	// fin is closed now, so the default listing omits it.
	list, err := st.ListTasks(ListOpts{})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range list {
		if v.Status == "fin" {
			t.Fatalf("closed-kind task %q listed; SQL closed list not swapped", v.Slug)
		}
	}

	// An invalid config is refused and the current one stays in force.
	bad := &Config{Statuses: []StatusDef{{Name: "only", Kind: KindOpen}}}
	if err := st.SetConfig(bad); err == nil {
		t.Fatal("SetConfig accepted an invalid config")
	}
	if got := st.Config().Names(); !reflect.DeepEqual(got, []string{"todo", "fin"}) {
		t.Fatalf("Config() after a rejected SetConfig = %v", got)
	}

	// nil restores the defaults.
	if err := st.SetConfig(nil); err != nil {
		t.Fatal(err)
	}
	if got := st.Config(); !reflect.DeepEqual(got, DefaultConfig()) {
		t.Fatalf("Config() after SetConfig(nil) = %v", got.Names())
	}
}

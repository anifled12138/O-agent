package core

import (
	"context"
	"reflect"
	"testing"
)

func TestManagerStartsDependenciesFirst(t *testing.T) {
	var started []string
	host := NewHost()
	manager := NewManager(host)
	for _, p := range []*Component{
		{Info: Manifest{ID: "agent", Requires: []string{"provider"}}, StartFn: func(context.Context) error { started = append(started, "agent"); return nil }},
		{Info: Manifest{ID: "provider", Requires: []string{"storage"}}, StartFn: func(context.Context) error { started = append(started, "provider"); return nil }},
		{Info: Manifest{ID: "storage"}, StartFn: func(context.Context) error { started = append(started, "storage"); return nil }},
	} {
		if err := manager.Register(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := manager.StartAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"storage", "provider", "agent"}; !reflect.DeepEqual(started, want) {
		t.Fatalf("start order %v, want %v", started, want)
	}
}

func TestManagerRejectsCycles(t *testing.T) {
	manager := NewManager(NewHost())
	_ = manager.Register(&Component{Info: Manifest{ID: "a", Requires: []string{"b"}}})
	_ = manager.Register(&Component{Info: Manifest{ID: "b", Requires: []string{"a"}}})
	if err := manager.StartAll(context.Background()); err == nil {
		t.Fatal("expected dependency cycle error")
	}
}

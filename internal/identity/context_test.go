package identity

import (
	"context"
	"sync"
	"testing"
)

func TestStateIsolationDerivedContextsAndValueCopies(t *testing.T) {
	Update(t.Context(), Snapshot{Name: "ignored"})
	if FromContext(t.Context()) != (Snapshot{}) {
		t.Fatal("state appeared without WithState")
	}
	ctx := WithState(t.Context())
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	value := Snapshot{Status: "matched", Name: "First", ID: "hash"}
	Update(ctx, value)
	value.Name = "mutated caller"
	copy := FromContext(child)
	copy.Name = "mutated reader"
	if FromContext(ctx).Name != "First" || FromContext(WithState(ctx)) != (Snapshot{}) {
		t.Fatal("request isolation or snapshot ownership lost")
	}
}

func TestConcurrentSnapshotsStayWhole(t *testing.T) {
	ctx := WithState(t.Context())
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() {
			for i := range 100 {
				name := "even"
				if i%2 != 0 {
					name = "odd"
				}
				Update(ctx, Snapshot{Status: name, Name: name})
				snapshot := FromContext(ctx)
				if snapshot.Status != snapshot.Name {
					t.Error("torn snapshot")
				}
			}
		})
	}
	group.Wait()
}

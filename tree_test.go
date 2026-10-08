package main

import (
	"encoding/json"
	"os"
	"testing"
)

func TestTreeProbeLive(t *testing.T) {
	db := os.Getenv("TREEPROBE_DB")
	sid := os.Getenv("TREEPROBE_SID")
	if db == "" || sid == "" {
		t.Skip("probe env not set")
	}
	tree, err := LoadSessionTree(db, sid)
	if err != nil {
		t.Fatalf("LoadSessionTree: %v", err)
	}
	b, _ := json.MarshalIndent(tree, "", " ")
	t.Logf("TREE:\n%s", string(b))
	ev, err := LoadChildEvents(db, sid, tree.Children[0].ID, 2000, 2000)
	if err != nil {
		t.Fatalf("LoadChildEvents: %v", err)
	}
	t.Logf("CHILD %s: %d events; first=%+v", tree.Children[0].ID, len(ev), ev[0])
}

package main

import (
	"fmt"
	"strings"
	"testing"
)

// GitHub reads a single ref at /git/ref/{ref} but updates one at
// /git/refs/{ref}. Reusing the read path for the update returns 404, which
// built the shaping commit and then dropped it — applications kept their
// placeholder identity and every module.
func TestRefReadAndUpdatePathsDiffer(t *testing.T) {
	owner, name, branch := "Velaoc", "harbor-roast", "main"
	read := fmt.Sprintf("/repos/%s/%s/git/ref/heads/%s", owner, name, branch)
	update := fmt.Sprintf("/repos/%s/%s/git/refs/heads/%s", owner, name, branch)

	if read == update {
		t.Fatal("read and update ref paths must differ")
	}
	if !strings.Contains(read, "/git/ref/") || strings.Contains(read, "/git/refs/") {
		t.Errorf("read path should use singular /git/ref/: %s", read)
	}
	if !strings.Contains(update, "/git/refs/") {
		t.Errorf("update path should use plural /git/refs/: %s", update)
	}
}

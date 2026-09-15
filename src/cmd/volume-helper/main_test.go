package main

import (
	"reflect"
	"testing"
)

func TestMoveCopyArgumentsPreserveFilesystemContract(t *testing.T) {
	copyArguments, verifyArguments := moveCopyArguments("rsync://source/data/", "/pool/incoming")
	wantCopy := []string{
		"-aHAXS", "--numeric-ids", "--one-file-system", "--no-devices", "--delete", "--fsync",
		"rsync://source/data/", "/pool/incoming/",
	}
	wantVerify := []string{
		"-aHAXS", "--numeric-ids", "--one-file-system", "--no-devices", "--delete",
		"--checksum", "--dry-run", "--itemize-changes", "rsync://source/data/", "/pool/incoming/",
	}
	if !reflect.DeepEqual(copyArguments, wantCopy) {
		t.Fatalf("copy arguments = %#v", copyArguments)
	}
	if !reflect.DeepEqual(verifyArguments, wantVerify) {
		t.Fatalf("verify arguments = %#v", verifyArguments)
	}
}

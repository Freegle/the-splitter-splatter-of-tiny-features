package main

import "testing"

func TestFeaturiseCommand_Registered(t *testing.T) {
	if _, ok := commands["featurise"]; !ok {
		t.Fatal(`"featurise" command not registered`)
	}
}

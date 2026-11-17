package main

import (
	"fmt"
	"testing"
)

func TestScenario(t *testing.T) {
	for _, user := range users {
		fmt.Printf("%d %s %s %s\n", user.id, user.login, user.name, user.email)
	}
	fmt.Println("viewer", viewerLogin)
	fmt.Println("epoch", stateEpochISO)
}

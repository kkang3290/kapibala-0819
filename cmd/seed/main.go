package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"taskboard/internal/configenv"
	"taskboard/internal/parameters"
	"taskboard/internal/store"
)

func main() {
	databaseURLDefault, err := configenv.String("DATABASE_URL", "postgres://taskboard:taskboard@localhost:54329/taskboard?sslmode=disable")
	if err != nil {
		fatal(err)
	}
	databaseURL := flag.String("database-url", databaseURLDefault, "PostgreSQL connection URL")
	flag.Parse()
	ctx := context.Background()
	dataStore, err := store.Open(ctx, *databaseURL)
	if err != nil {
		fatal(err)
	}
	defer dataStore.Close()
	if err := dataStore.Migrate(ctx); err != nil {
		fatal(err)
	}

	examples := []store.CreateTaskInput{
		{
			GroupName:      "华东客户群",
			GroupOverrides: parameters.Values{"channel": "sms", "region": "east", "signature": ""},
			BaseParameters: parameters.Values{"channel": "email", "region": "cn", "retries": 1, "signature": "ACME"},
			Steps: []store.StepInput{
				{Overrides: parameters.Values{"template": "welcome", "sender": "service-a"}},
				{Overrides: parameters.Values{"sender": "", "retries": 2}},
				{Overrides: parameters.Values{"template": "follow-up"}},
			},
		},
		{
			GroupName:      "VIP 客户群",
			GroupOverrides: parameters.Values{"priority": "high"},
			BaseParameters: parameters.Values{"channel": "push", "priority": "normal"},
			Steps: []store.StepInput{
				{Overrides: parameters.Values{"template": "vip-offer"}},
				{Overrides: parameters.Values{"template": ""}},
			},
		},
	}
	for _, example := range examples {
		id, err := dataStore.CreateTask(ctx, example)
		if err != nil {
			fatal(err)
		}
		fmt.Printf("created task #%d\n", id)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

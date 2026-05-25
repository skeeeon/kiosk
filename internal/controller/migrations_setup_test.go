package controller

// Side-effect imports that register both kiosk and controller-only
// migrations with the global PB AppMigrations list. setupApp in
// consumer_test.go runs the resulting set against a fresh per-test DB.
import (
	_ "github.com/skeeeon/kiosk/migrations"
	_ "github.com/skeeeon/kiosk/migrations/controller"
)

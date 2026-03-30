package main

import (
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/kelseyhightower/envconfig"

	"knative.dev/container-freezer/pkg/daemon"
	"knative.dev/container-freezer/pkg/freeze"
	pkglogging "knative.dev/pkg/logging"
)

type config struct {
	RuntimeType string `split_words:"true" required:"true"`
	APIKey      string `split_words:"true"` // optional; if set, clients must send Authorization: Bearer <key>

	// Logging configuration
	FreezerLoggingConfig string `split_words:"true"`
	FreezerLoggingLevel  string `split_words:"true"`
}

func main() {
	var env config
	if err := envconfig.Process("", &env); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	logger, _ := pkglogging.NewLogger(env.FreezerLoggingConfig, env.FreezerLoggingLevel)

	freezeThaw, err := freeze.NewCRIProvider(env.RuntimeType)
	if err != nil {
		log.Fatal(err)
	}

	http.ListenAndServe(":8080", &daemon.Handler{
		Freezer: freezeThaw,
		Thawer:  freezeThaw,
		Logger:  logger,
		APIKey:  env.APIKey,
	})
}

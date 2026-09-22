// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/kubeflow/notebooks/gke/internal/deploy"
)

func main() {
	configPath := flag.String("config", "", "Path to non-secret JSON deployment configuration")
	stage := flag.String("stage", "", "namespaces, isolation, applications, or edge")
	root := flag.String("repo-root", "..", "Repository root; default assumes execution from gke/")
	flag.Parse()
	file, err := os.Open(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()
	config, err := deploy.ReadConfig(file)
	if err != nil {
		log.Fatal(err)
	}
	absoluteRoot, err := filepath.Abs(*root)
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	resources, err := deploy.Render(ctx, absoluteRoot, *stage, config, deploy.Kustomize)
	if err != nil {
		log.Fatal(err)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(map[string]any{"apiVersion": "v1", "kind": "List", "items": resources}); err != nil {
		log.Fatal(err)
	}
}

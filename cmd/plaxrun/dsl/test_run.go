/*
 * Copyright 2021 Comcast Cable Communications Management, LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * SPDX-License-Identifier: Apache-2.0
 */

package dsl

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Comcast/plax/cmd/plaxrun/async"
	"github.com/Comcast/plax/junit"

	plaxDsl "github.com/Comcast/plax/dsl"
)

// Ctx is the context type
type Ctx struct {
	*plaxDsl.Ctx
}

// NewCtx builds a new Ctx
func NewCtx(ctx context.Context) *Ctx {
	return &Ctx{
		plaxDsl.NewCtx(ctx),
	}
}

// TestRun is the top-level type for a test run.
type TestRun struct {
	Name      string              `yaml:"name" json:"name"`
	Version   string              `yaml:"version" json:"version"`
	Tests     TestDefMap          `yaml:"tests" json:"-"`
	Groups    TestGroupMap        `yaml:"groups" json:"-"`
	Params    TestParamBindingMap `yaml:"params" json:"-"`
	trps      *TestRunParams      `json:"-"`
	tfs       []*async.TaskFunc   `json:"-"`
	TestSuite []*junit.TestSuite  `xml:"testsuite" json:"testsuite"`
	Total     int                 `xml:"tests,attr" json:"tests"`
	Skipped   int                 `xml:"skipped,attr" json:"skipped"`
	Failures  int                 `xml:"failures,attr" json:"failures"`
	Errors    int                 `xml:"errors,attr" json:"errors"`
	Started   time.Time           `xml:"started,attr" json:"timestamp"`
	Time      time.Duration       `xml:"time,attr" json:"time"`
}

// NewTestRun makes a new TestRun with the given TestRunParams
func NewTestRun(ctx *Ctx, trps *TestRunParams) (*TestRun, error) {
	tr := TestRun{
		TestSuite: make([]*junit.TestSuite, 0),
		Started:   time.Now().UTC(),
	}

	if trps.Dir == nil {
		return nil, fmt.Errorf("TestRunParams.Dir is nil")
	}

	ctx.Dir = *trps.Dir
	ctx.LogLevel = *trps.LogLevel
	ctx.IncludeDirs = trps.IncludeDirs
	ctx.Redact = *trps.Redact

	var filename string
	if trps.Filename != nil {
		filename = *trps.Filename
	}

	// Add the test run directory to the end of the includeDirs.
	dir, err := filepath.Abs(filepath.Dir(filename))
	if err != nil {
		return nil, fmt.Errorf("failed to find path to test run file: %w", err)
	}
	ctx.IncludeDirs = append(ctx.IncludeDirs, dir)

	bs, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to read test runner configuration file: %w", err)
	}

	ctx.Redactf("Test Bindings: %v\n", trps.Bindings)

	err = os.Chdir(*trps.Dir)
	if err != nil {
		return nil, fmt.Errorf("failed to change directory: %w", err)
	}

	ctx.IncludeDirs = append(ctx.IncludeDirs, *trps.Dir)

	bs, err = plaxDsl.IncludeYAML(ctx.Ctx, bs)
	if err != nil {
		return nil, fmt.Errorf("failed to process include YAML: %w", err)
	}

	if err := yaml.Unmarshal(bs, &tr); err != nil {
		return nil, fmt.Errorf("test runner configuration parse error: %w", err)
	}

	ctx.Logdf("TestRun: %v\n", tr)

	tr.trps = trps

	tfs, err := trps.Groups.getTaskFuncs(ctx.Ctx, tr)
	if err != nil {
		return nil, fmt.Errorf("failed to process test groups to execute: %w", err)
	}

	tr.tfs = append(tr.tfs, tfs...)

	if trps.SuiteName != nil && *trps.SuiteName != "" {
		testSuite := TestSuiteRef{
			name:  *trps.SuiteName,
			tests: trps.Tests,
		}
		tf, err := testSuite.getTaskFunc(ctx.Ctx, tr)
		if err != nil {
			return nil, fmt.Errorf("failed to process tests to execute: %w", err)
		}

		tr.tfs = append(tr.tfs, tf)
	} else {
		tfs, err = trps.Tests.getTaskFuncs(ctx.Ctx, tr)
		if err != nil {
			return nil, fmt.Errorf("failed to process tests to execute: %w", err)
		}

		tr.tfs = append(tr.tfs, tfs...)
	}

	return &tr, nil
}

// HasError determines if any of the TaskResults is an error
func (trr TestRun) HasError() bool {
	return trr.Errors > 0
}

func (tr *TestRun) Finish(message ...string) {
	now := time.Now().UTC()
	time := now.Sub(tr.Started)
	tr.Time = time
}

// Exec the TestRun
func (tr *TestRun) Exec(ctx *Ctx) error {
	taskResults, err := async.Sequential(ctx, tr.tfs...)
	if err != nil {
		return fmt.Errorf("failed to execute tasks: %w", err)
	}

	for _, taskResult := range taskResults {
		if ts, ok := taskResult.Result.(*junit.TestSuite); ok {
			if ts != nil {
				tr.TestSuite = append(tr.TestSuite, ts)
				tr.Total += ts.Total
				tr.Skipped += ts.Skipped
				tr.Failures += ts.Failures
				tr.Errors += ts.Errors
			}
		}
	}

	tr.Finish()

	if *tr.trps.EmitJSON {
		// Write the JSON.
		js, err := json.MarshalIndent(tr, "", "  ")
		if err != nil {
			log.Fatal(err)
		}

		fmt.Printf("%s\n", js)
	} else {
		bs, err := xml.MarshalIndent(tr, "", "  ")
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%s\n", bs)
	}

	if taskResults.HasError() {
		ctx.Logdf("TaskResult Error: %s", taskResults.Error())
		return fmt.Errorf(taskResults.Error())
	}

	return nil
}

// IncludeDirList are the directories to search when YAML-including.
//
// We make an explicit type to enable flag.Var to parse multiple
// parameters.
type IncludeDirList []string

// String representation
func (idl *IncludeDirList) String() string {
	return "value=[Directory Path]"
}

// Set the include directories
func (idl *IncludeDirList) Set(value string) error {
	*idl = append(*idl, value)
	return nil
}

// TestRunParams used to exec a TestRun
type TestRunParams struct {
	Bindings    plaxDsl.Bindings
	Groups      TestGroupList
	Tests       TestList
	SuiteName   *string
	IncludeDirs IncludeDirList
	Filename    *string
	Dir         *string
	EmitJSON    *bool
	Verbose     *bool
	LogLevel    *string
	Labels      *string
	Priority    *int
	Redact      *bool
}

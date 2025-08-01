// Copyright 2022 The CUE Authors
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

package github

import (
	"list"
	"cue.dev/x/githubactions"
)

// The trybot workflow.
workflows: trybot: _repo.bashWorkflow & {
	name: _repo.trybot.name

	on: {
		push: {
			branches: list.Concat([[_repo.testDefaultBranch], _repo.protectedBranchPatterns]) // do not run PR branches
			"tags-ignore": [_repo.releaseTagPattern]
		}
		// Note that pull_request_target gives PR CI jobs full access to our secrets,
		// which is necessary to fetch dependencies from the registry via NOTCUECKOO_CUE_TOKEN.
		// Giving access to secrets is OK given that we must approve PR jobs to run on CI,
		// which mirrors the approval workflow for CI on Gerrit.
		pull_request_target: {}
	}

	jobs: {
		test: {
			strategy: {
				"fail-fast": false
				matrix: {
					(_repo.matrixRunnerName): [_repo.linuxMachine, _repo.macosMachine, _repo.windowsMachine]
					(_repo.matrixGoVersionName): [_repo.previousGo, _repo.latestGo]
				}
			}
			"runs-on": "${{ \(_repo.matrixRunnerExpr) }}"

			let installGo = _repo.installGo & {
				#setupGo: with: "go-version": "${{ \(_repo.matrixGoVersionExpr) }}"
				_
			}

			// Only run the trybot workflow if we have the trybot trailer, or
			// if we have no special trailers. Note this condition applies
			// after and in addition to the "on" condition above.
			if: "\(_repo.containsTrybotTrailer) || ! \(_repo.containsDispatchTrailer)"

			steps: [
				for v in _repo.checkoutCode {v},
				for v in installGo {v},
				for v in _repo.setupCaches {v},

				_repo.loginCentralRegistry,

				_repo.earlyChecks & {
					// These checks don't vary based on the Go version or OS,
					// so we only need to run them on one of the matrix jobs.
					if: _repo.isLatestGoLinux
				},
				_goTest & {
					if: "\(_repo.isProtectedBranch) || !\(_repo.isLatestGoLinux)"
				},
				_goTestRace,
				_goTest32bit,
				_goTestWasm,
				for v in _e2eTestSteps {v},
				for v in _goChecks {v},
				_checkTags,
				// Run code generation towards the very end, to ensure it succeeds and makes no changes.
				// Note that doing this before any Go tests or checks may lead to test cache misses,
				// as Go uses modtimes to approximate whether files have been modified.
				// Moveover, Go test failures on CI due to changed generated code are very confusing
				// as the user might not notice that checkGitClean is also failing towards the end.
				_goGenerate,
				_repo.checkGitClean,
			]
		}
	}

	_goGenerate: githubactions.#Step & {
		name: "Generate"
		run:  "go generate ./..."
		// The Go version corresponds to the precise version specified in
		// the matrix. Skip windows for now until we work out why re-gen is flaky
		if: _repo.isLatestGoLinux
	}

	_goTest: githubactions.#Step & {
		name: "Test"
		run:  "go test ./..."
	}

	_e2eTestSteps: [... githubactions.#Step & {
		// The end-to-end tests require a github token secret and are a bit slow,
		// so we only run them on pushes to protected branches and on one
		// environment in the source repo.
		if: "github.repository == '\(_repo.githubRepositoryPath)' && (\(_repo.isProtectedBranch) || \(_repo.isTestDefaultBranch)) && \(_repo.isLatestGoLinux)"
	}] & [
		// Two setup steps per the upstream docs:
		// https://github.com/google-github-actions/setup-gcloud#service-account-key-json
		{
			name: "gcloud auth for end-to-end tests"
			id:   "auth"
			uses: "google-github-actions/auth@v2"
			// E2E_GCLOUD_KEY is a key for the service account cue-e2e-ci,
			// which has the Artifact Registry Repository Administrator role.
			with: credentials_json: "${{ secrets.E2E_GCLOUD_KEY }}"
		},
		{
			name: "gcloud setup for end-to-end tests"
			uses: "google-github-actions/setup-gcloud@v2"
		},
		{
			name: "End-to-end test"
			env: {
				// E2E_PORCUEPINE_CUE_TOKEN is a token generated on registry.cue.works
				// as the GitHub porcuepine user, with description "e2e cue repo".
				CUE_TEST_TOKEN: "${{ secrets.E2E_PORCUEPINE_CUE_TOKEN }}"
			}
			// Our regular tests run with both `go test ./...` and `go test -race ./...`.
			// The end-to-end tests should only be run once, given the slowness and API rate limits.
			// We want to catch any data races they spot as soon as possible, and they aren't CPU-bound,
			// so running them only with -race seems reasonable.
			run: """
				cd internal/_e2e
				go test -race
				"""
		},
	]

	_goChecks: [...githubactions.#Step & {
		// These checks can vary between platforms, as different code can be built
		// based on GOOS and GOARCH build tags.
		// However, CUE does not have any such build tags yet, and we don't use
		// dependencies that vary wildly between platforms.
		// For now, to save CI resources, just run the checks on one matrix job.
		if: _repo.isLatestGoLinux
	}] & [
		_repo.goChecks,
		{
			name: "Verify the end-to-end tests still build"
			// Ensure that the end-to-end tests in ./internal/_e2e, which are only run
			// on pushes to protected branches, still build correctly before merging.
			"working-directory": "./internal/_e2e"
			run:                 "go test -run=-"
		},
		// Note that we don't want tooling dependencies in the go.mod file,
		// given how many downstreams rely on the cue module having few dependencies.
		_repo.staticcheck & {#in: modfile: "internal/tools.mod"},
	]

	_checkTags: githubactions.#Step & {
		// Ensure that GitHub and Gerrit agree on the full list of available tags.
		// This way, if there is any discrepancy, we will get a useful diff.
		//
		// We use `git ls-remote` to list all tags from each remote git repository
		// because it does not depend on custom REST API endpoints and is very fast.
		// Note that it sorts tag names as strings, which is not the best, but works OK.
		if:   "(\(_repo.isProtectedBranch) || \(_repo.isTestDefaultBranch)) && \(_repo.isLatestGoLinux)"
		name: "Check all git tags are available"
		run: """
			cd $(mktemp -d)

			git ls-remote --tags https://github.com/cue-lang/cue >github.txt
			echo "GitHub tags:"
			sed 's/^/    /' github.txt

			git ls-remote --tags https://review.gerrithub.io/cue-lang/cue >gerrit.txt

			if ! diff -u github.txt gerrit.txt; then
				echo "GitHub and Gerrit do not agree on the list of tags!"
				echo "Did you forget about refs/attic branches? https://github.com/cue-lang/cue/wiki/Notes-for-project-maintainers"
				exit 1
			fi
			"""
	}

	_goTestRace: githubactions.#Step & {
		// Windows and Mac on CI are slower than Linux, and most data races are not specific
		// to any OS or Go version in particular, so only run all tests with -race on Linux
		// to not slow down CI unnecessarily.
		if:   _repo.isLatestGoLinux
		name: "Test with -race"
		env: GORACE: "atexit_sleep_ms=10" // Otherwise every Go package being tested sleeps for 1s; see https://go.dev/issues/20364.
		run: "go test -race ./..."
	}

	_goTest32bit: githubactions.#Step & {
		// Ensure that the entire build and all tests succeed on a 32-bit platform as well.
		// This should catch if any of the code or test cases rely on bit sizes,
		// such as int being 64 bits, which could cause portability bugs for 32-bit platforms.
		// While GOARCH=386 isn't particularly popular anymore, it can run on our amd64 Linux runner.
		//
		// Running just the short tests is enough for now.
		// We skip this step when testing CLs and PRs, as Linux on the latest Go is the slowest
		// job in the matrix due to the use of `go test -race`. 32-bit bugs should be rare,
		// so them only getting caught once a patch is merged into master is not a big problem.
		if:   "(\(_repo.isProtectedBranch) || \(_repo.isTestDefaultBranch)) && \(_repo.isLatestGoLinux)"
		name: "Test on 32 bits"
		env: GOARCH: "386"
		run: "go test -short ./..."
	}

	_goTestWasm: githubactions.#Step & {
		name: "Test with -tags=cuewasm"
		// The wasm interpreter is only bundled into cmd/cue with the cuewasm build tag.
		// Test the related packages with the build tag enabled as well.
		run: "go test -tags cuewasm ./cmd/cue/cmd ./cue/interpreter/wasm"
	}
}

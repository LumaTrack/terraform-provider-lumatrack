package provider

import (
	"fmt"
	"net/http"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// regexpQuote matches a diagnostic message literally. Terraform wraps
// diagnostics across lines, so the assertions target short distinctive
// fragments rather than whole sentences.
func regexpQuote(s string) *regexp.Regexp {
	return regexp.MustCompile(regexp.QuoteMeta(s))
}

// Every case below drives real Terraform against the stub server from
// client_test.go. Nothing here reaches lumatrack.io.
func testProviderFactories() map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		"lumatrack": providerserver.NewProtocol6WithError(New("test")()),
	}
}

func providerConfig(endpoint string) string {
	return fmt.Sprintf(`
provider "lumatrack" {
  endpoint = %[1]q
  api_key  = "lmt_test_key"
}
`, endpoint)
}

func TestAccRunResourceRecordsARun(t *testing.T) {
	s := newStub(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: providerConfig(s.server.URL) + `
resource "lumatrack_run" "nightly" {
  automation       = "os-patching"
  status           = "success"
  duration_seconds = 142
  units            = 240
  external_id      = "tf-apply-99412"
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lumatrack_run.nightly", "id", "run_01ABC"),
					resource.TestCheckResourceAttr("lumatrack_run.nightly", "automation", "os-patching"),
					resource.TestCheckResourceAttr("lumatrack_run.nightly", "status", "success"),
					resource.TestCheckResourceAttr("lumatrack_run.nightly", "held", "false"),
					resource.TestCheckResourceAttr("lumatrack_run.nightly", "deduplicated", "false"),
					// Unset by the operator, stamped by the provider so the
					// ledger can tell Terraform-reported runs apart.
					resource.TestCheckResourceAttr("lumatrack_run.nightly", "source", "terraform"),
				),
			},
		},
	})

	if len(s.requests) != 1 {
		t.Fatalf("want exactly 1 ingest request, got %d", len(s.requests))
	}
	body := s.requests[0].body
	if body["automation"] != "os-patching" {
		t.Errorf("automation: got %v", body["automation"])
	}
	if body["duration_seconds"] != float64(142) {
		t.Errorf("duration_seconds: got %v", body["duration_seconds"])
	}
	if body["units"] != float64(240) {
		t.Errorf("units: got %v", body["units"])
	}
	if body["external_id"] != "tf-apply-99412" {
		t.Errorf("external_id: got %v", body["external_id"])
	}
	if body["source"] != "terraform" {
		t.Errorf("source: got %v", body["source"])
	}
}

// The whole point of the product: a failed run costs money and saves nothing.
// Reporting one has to work as smoothly as reporting a success.
func TestAccRunResourceRecordsAFailure(t *testing.T) {
	s := newStub(t)
	s.body = `{"run":{"id":"run_01FAIL","held":false,"status":"failure"},"deduplicated":false}`

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: providerConfig(s.server.URL) + `
resource "lumatrack_run" "nightly" {
  automation     = "os-patching"
  status         = "failure"
  failure_reason = "auth/credential"
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lumatrack_run.nightly", "id", "run_01FAIL"),
					resource.TestCheckResourceAttr("lumatrack_run.nightly", "status", "failure"),
				),
			},
		},
	})

	body := s.requests[0].body
	if body["status"] != "failure" {
		t.Errorf("status: want failure, got %v", body["status"])
	}
	if body["failure_reason"] != "auth/credential" {
		t.Errorf("failure_reason: got %v", body["failure_reason"])
	}
}

// status defaults to success, and metadata rides along as a JSON object.
func TestAccRunResourceDefaultsAndMetadata(t *testing.T) {
	s := newStub(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: providerConfig(s.server.URL) + `
resource "lumatrack_run" "nightly" {
  automation = "os-patching"
  metadata = {
    workspace = "prod"
    hosts     = "240"
  }
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lumatrack_run.nightly", "status", "success"),
					resource.TestCheckResourceAttr("lumatrack_run.nightly", "metadata.workspace", "prod"),
					// Unset optionals must stay null in state, never 0.
					resource.TestCheckNoResourceAttr("lumatrack_run.nightly", "duration_seconds"),
				),
			},
		},
	})

	body := s.requests[0].body
	if body["status"] != "success" {
		t.Errorf("status: want the success default, got %v", body["status"])
	}
	metadata, ok := body["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata: want an object, got %T", body["metadata"])
	}
	if metadata["workspace"] != "prod" || metadata["hosts"] != "240" {
		t.Errorf("metadata: got %v", metadata)
	}
	if _, present := body["duration_seconds"]; present {
		t.Errorf("duration_seconds must be omitted when unset, got %v", body["duration_seconds"])
	}
}

// With no external_id in config the provider generates one, so a re-apply of
// unchanged config reports nothing new instead of double-counting.
func TestAccRunResourceGeneratesAnExternalIDAndIsStableOnReapply(t *testing.T) {
	s := newStub(t)
	config := providerConfig(s.server.URL) + `
resource "lumatrack_run" "nightly" {
  automation = "os-patching"
}
`

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.TestCheckResourceAttrSet(
					"lumatrack_run.nightly", "external_id"),
			},
			{
				// Same config again: no diff, so no second run.
				Config:   config,
				PlanOnly: true,
			},
		},
	})

	if len(s.requests) != 1 {
		t.Fatalf("re-applying unchanged config must not report a second run, got %d requests", len(s.requests))
	}
	if s.requests[0].body["external_id"] == nil || s.requests[0].body["external_id"] == "" {
		t.Errorf("external_id must be generated when unset, got %v", s.requests[0].body["external_id"])
	}
}

// A run is an immutable fact. Changing what it reports means a different run
// happened, so the resource is replaced and a second event is recorded.
func TestAccRunResourceReplacesOnChange(t *testing.T) {
	s := newStub(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: providerConfig(s.server.URL) + `
resource "lumatrack_run" "nightly" {
  automation  = "os-patching"
  external_id = "run-one"
}
`,
			},
			{
				Config: providerConfig(s.server.URL) + `
resource "lumatrack_run" "nightly" {
  automation  = "os-patching"
  external_id = "run-two"
}
`,
			},
		},
	})

	if len(s.requests) != 2 {
		t.Fatalf("want 2 runs recorded across the two applies, got %d", len(s.requests))
	}
	if s.requests[0].body["external_id"] != "run-one" || s.requests[1].body["external_id"] != "run-two" {
		t.Errorf("external ids: got %v then %v",
			s.requests[0].body["external_id"], s.requests[1].body["external_id"])
	}
}

// An unknown slug is the most common first-run mistake. The server names it;
// the provider has to pass that message through to the operator verbatim.
func TestAccRunResourceSurfacesAnUnknownSlug(t *testing.T) {
	s := newStub(t)
	s.status = http.StatusNotFound
	s.body = `{"error":"No automation with slug 'nope'. See GET /api/v1/automations."}`

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: providerConfig(s.server.URL) + `
resource "lumatrack_run" "nightly" {
  automation = "nope"
}
`,
				ExpectError: regexpQuote("No automation with slug 'nope'"),
			},
		},
	})
}

// A bad status is caught at plan time, before anything is sent.
func TestAccRunResourceRejectsAnUnknownStatus(t *testing.T) {
	s := newStub(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: providerConfig(s.server.URL) + `
resource "lumatrack_run" "nightly" {
  automation = "os-patching"
  status     = "green"
}
`,
				ExpectError: regexpQuote("Attribute status value must be one of"),
			},
		},
	})

	if len(s.requests) != 0 {
		t.Errorf("an invalid status must never be sent, got %d requests", len(s.requests))
	}
}

// A 202 means the run was stored but held over the plan's monthly cap: held
// runs earn nothing, so the operator needs it visible in state.
func TestAccRunResourceExposesHeldRuns(t *testing.T) {
	s := newStub(t)
	s.status = http.StatusAccepted
	s.body = `{"run":{"id":"run_01HELD","held":true},"deduplicated":false}`

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: providerConfig(s.server.URL) + `
resource "lumatrack_run" "nightly" {
  automation = "os-patching"
}
`,
				Check: resource.TestCheckResourceAttr("lumatrack_run.nightly", "held", "true"),
			},
		},
	})
}

// The provider reads its credentials from the environment so a key never has
// to sit in a .tf file.
func TestAccProviderReadsEndpointAndKeyFromEnv(t *testing.T) {
	s := newStub(t)
	t.Setenv("LUMATRACK_ENDPOINT", s.server.URL)
	t.Setenv("LUMATRACK_API_KEY", "lmt_env_key")

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: `
provider "lumatrack" {}

resource "lumatrack_run" "nightly" {
  automation = "os-patching"
}
`,
			},
		},
	})

	if len(s.requests) != 1 {
		t.Fatalf("want 1 request, got %d", len(s.requests))
	}
	if s.requests[0].auth != "Bearer lmt_env_key" {
		t.Errorf("auth: want the env key, got %q", s.requests[0].auth)
	}
}

// Without a key anywhere, the provider says so at configure time rather than
// sending an unauthenticated request.
func TestAccProviderRequiresAnAPIKey(t *testing.T) {
	s := newStub(t)
	t.Setenv("LUMATRACK_API_KEY", "")

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
provider "lumatrack" {
  endpoint = %q
}

resource "lumatrack_run" "nightly" {
  automation = "os-patching"
}
`, s.server.URL),
				ExpectError: regexpQuote("Missing LumaTrack API key"),
			},
		},
	})

	if len(s.requests) != 0 {
		t.Errorf("must not send an unauthenticated request, got %d", len(s.requests))
	}
}

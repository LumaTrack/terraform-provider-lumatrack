# Terraform provider for LumaTrack

Reports automation run events to [LumaTrack](https://lumatrack.io/?utm_source=terraform-provider),
the system of record for automation value. One resource in your config and each
apply lands in a ledger that prices what the automation saved against the manual
baseline you defined.

The provider does one thing: it records a run. There's no resource here for
creating automations or reading your value numbers, because a pipeline that
reports runs should be able to run with an ingest-only key that cannot read your
ledger back.

## Usage

```hcl
terraform {
  required_providers {
    lumatrack = {
      source  = "LumaTrack/lumatrack"
      version = "~> 0.1"
    }
  }
}

provider "lumatrack" {
  # Or set LUMATRACK_ENDPOINT and LUMATRACK_API_KEY.
  endpoint = "https://lumatrack.io"
  api_key  = var.lumatrack_api_key
}

resource "lumatrack_run" "network_baseline" {
  automation       = "network-device-baseline"
  status           = "success"
  duration_seconds = 142
  units            = 240

  metadata = {
    workspace = terraform.workspace
    commit    = var.git_sha
  }
}
```

`units = 240` is what makes the numbers mean something here. If this config
baselines 240 switches and your baseline says a network engineer spends four
minutes per device by hand, the ledger books 16 hours against that engineer's
loaded rate. One run with `units = 240` is the shape you want. Two hundred and
forty separate runs would each consume an event against your plan's monthly cap
and tell you the same thing.

### Reporting failures

A ledger that only hears about successes flatters itself. Failed runs cost money
and save nothing, and LumaTrack prices that honestly, so report them.

Terraform makes this slightly awkward, because a resource in a failed apply
never gets created. Drive `status` from your pipeline instead:

```hcl
variable "run_status" {
  type        = string
  default     = "success"
  description = "success, failure, skipped or cancelled, set by the pipeline"
}

variable "run_reason" {
  type        = string
  default     = ""
  description = "root cause when the run failed, e.g. auth/credential"
}

resource "lumatrack_run" "network_baseline" {
  automation     = "network-device-baseline"
  status         = var.run_status
  failure_reason = var.run_reason
  external_id    = var.pipeline_run_id
}
```

Your CI job then applies the real work, and on either outcome applies a second
small config (or a second workspace holding only this resource) with
`run_status` set from the exit code. In GitLab CI that's an `after_script` with
`$CI_JOB_STATUS`; in GitHub Actions it's a step with `if: always()` reading
`job.status`. LumaTrack also publishes a
[GitHub Action](https://github.com/LumaTrack/report-run) and a GitLab CI
component that do the reporting step for you, and either pairs fine with this
provider.

### Idempotency

Leave `external_id` unset and the provider generates one, storing it in state.
Re-applying unchanged config then records nothing new, because the ledger
recognizes the replay and answers 200 instead of writing a second run.

That guarantee lives in your state file. If state is lost between the POST and
the write, the next apply generates a fresh id and records a second run for work
that happened once. Set `external_id` to your pipeline's own run id when you
want exactly-once reporting that survives a lost state file:

```hcl
external_id = "gitlab-${var.ci_pipeline_id}-${var.ci_job_id}"
```

### What a run resource does and doesn't do

A recorded run is append-only evidence, so the resource behaves like the fact it
represents:

- Changing any attribute replaces the resource and records a second run. A
  different automation, status or duration means a different run happened.
- `terraform destroy` drops the resource from state and leaves the recorded run
  in LumaTrack. The run happened. Deleting evidence is a role-gated decision in
  the app.
- `terraform refresh` reads nothing back. An ingest-only key acknowledges a
  replay rather than echoing stored evidence, which is the point of deploying
  one inside a runner.

## Run tasks are the richer integration

Terraform's own signal for "did this apply succeed" is the run task, which HCP
Terraform and Terraform Enterprise call at pre-plan, post-plan, pre-apply and
post-apply with the real outcome. A LumaTrack run task sees the failures this
resource can't, without you threading a status variable through your pipeline.

This provider is the version you can adopt today with nothing but a key: it
works on Terraform CLI, in any CI system, and on Community Edition, where run
tasks aren't available. If you're on HCP Terraform, ask us about the run task
integration at [lumatrack.io](https://lumatrack.io/?utm_source=terraform-provider).

## Provider configuration

| Argument | Environment | Notes |
|---|---|---|
| `endpoint` | `LUMATRACK_ENDPOINT` | Your LumaTrack host. Defaults to `https://lumatrack.io` |
| `api_key` | `LUMATRACK_API_KEY` | Organization-scoped key from Settings, API keys. Marked sensitive |

An ingest-only key is enough to record runs and is the one to deploy in a
pipeline. If it leaks, an attacker can post junk runs, and they cannot read or
restate your ledger.

Rate limits are counted per organization across every key. On a 429 that carries
`Retry-After`, the provider waits and retries once. A monthly event-cap 429
carries no `Retry-After` and fails the apply with the server's explanation,
because that ceiling won't move today.

## Requirements

- Terraform 1.0 or later, or OpenTofu
- Go 1.26 to build from source
- A LumaTrack organization with an automation slug to report against. The free
  tier works. List your slugs with `GET /api/v1/automations`

## Development

```sh
go build ./...
go test ./...          # unit tests plus resource tests against a local stub
gofmt -l .             # must print nothing
go vet ./...
```

The resource tests drive real Terraform against an `httptest` stub server, so
they need no credentials and never reach a LumaTrack instance. The test
framework downloads a Terraform binary the first time it runs.

To try the provider against a local build before it's published, add a dev
override to `~/.terraformrc`:

```hcl
provider_installation {
  dev_overrides {
    "LumaTrack/lumatrack" = "/path/to/your/gopath/bin"
  }
  direct {}
}
```

## License

MIT. See [LICENSE](LICENSE).

---

Built by [LumaTrack](https://lumatrack.io/?utm_source=terraform-provider), which
turns automation runs into a defensible ROI number.

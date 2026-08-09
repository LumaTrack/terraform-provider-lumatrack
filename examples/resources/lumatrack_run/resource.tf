# One run for the whole apply, with units set to the number of devices it
# touched. The ledger prices that as fixed setup plus marginal minutes per
# device, against the baseline you defined in LumaTrack.
resource "lumatrack_run" "network_baseline" {
  automation       = "network-device-baseline"
  status           = var.run_status
  failure_reason   = var.run_reason
  duration_seconds = var.run_duration_seconds
  units            = 240

  # Your pipeline's own run id makes reporting exactly-once even if the
  # Terraform state file is lost.
  external_id = "gitlab-${var.ci_pipeline_id}-${var.ci_job_id}"

  metadata = {
    workspace = terraform.workspace
    commit    = var.git_sha
  }
}

variable "run_status" {
  type        = string
  default     = "success"
  description = "success, failure, skipped or cancelled, set by the pipeline from its exit code"
}

variable "run_reason" {
  type        = string
  default     = ""
  description = "Root cause when the run failed, e.g. auth/credential"
}

variable "run_duration_seconds" {
  type        = number
  default     = null
  description = "Wall-clock runtime. Leave null rather than sending a guess."
}

variable "ci_pipeline_id" {
  type = string
}

variable "ci_job_id" {
  type = string
}

variable "git_sha" {
  type = string
}

output "lumatrack_run_id" {
  value       = lumatrack_run.network_baseline.id
  description = "The recorded run id (run_...)"
}

output "lumatrack_run_held" {
  value       = lumatrack_run.network_baseline.held
  description = "True when the run was held over the plan's monthly event cap, earning nothing until the plan is raised"
}

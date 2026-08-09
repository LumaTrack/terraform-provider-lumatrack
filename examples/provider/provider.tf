terraform {
  required_providers {
    lumatrack = {
      source  = "LumaTrack/lumatrack"
      version = "~> 0.1"
    }
  }
}

# Both arguments fall back to LUMATRACK_ENDPOINT and LUMATRACK_API_KEY, which is
# how a pipeline should supply them.
provider "lumatrack" {
  endpoint = "https://lumatrack.io"
  api_key  = var.lumatrack_api_key
}

variable "lumatrack_api_key" {
  type        = string
  sensitive   = true
  description = "Ingest-only API key from Settings, API keys"
}

package provider

import (
	"context"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Environment variables the provider falls back to, so a key never has to be
// written into a .tf file or a state-visible variable.
const (
	envEndpoint = "LUMATRACK_ENDPOINT"
	envAPIKey   = "LUMATRACK_API_KEY"
)

var _ provider.Provider = &lumatrackProvider{}

type lumatrackProvider struct {
	version string
}

// New returns the provider factory the plugin server and the tests both use.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &lumatrackProvider{version: version}
	}
}

type providerModel struct {
	Endpoint types.String `tfsdk:"endpoint"`
	APIKey   types.String `tfsdk:"api_key"`
}

func (p *lumatrackProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "lumatrack"
	resp.Version = p.version
}

func (p *lumatrackProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reports automation run events to LumaTrack, the system of record for automation value.",
		Attributes: map[string]schema.Attribute{
			"endpoint": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Base URL of your LumaTrack instance, e.g. `https://lumatrack.example.com`. " +
					"Defaults to `" + DefaultEndpoint + "`. May also be set with `" + envEndpoint + "`.",
			},
			"api_key": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "Organization-scoped API key from Settings, API keys. An **ingest-only** " +
					"key is enough to record runs and is the one to deploy here: if it leaks, an attacker can " +
					"post junk runs but cannot read or restate your ledger. May also be set with `" + envAPIKey + "`.",
			},
		},
	}
}

func (p *lumatrackProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var config providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// An unknown value here means it comes from something Terraform has not
	// resolved yet (another resource's output). Saying so beats sending an
	// empty key and reporting a 401.
	if config.Endpoint.IsUnknown() {
		resp.Diagnostics.AddAttributeError(
			path.Root("endpoint"),
			"Unknown LumaTrack endpoint",
			"The endpoint is not known at configure time. Set it to a literal value, or supply it with "+envEndpoint+".",
		)
	}
	if config.APIKey.IsUnknown() {
		resp.Diagnostics.AddAttributeError(
			path.Root("api_key"),
			"Unknown LumaTrack API key",
			"The API key is not known at configure time. Set it to a literal value, or supply it with "+envAPIKey+".",
		)
	}
	if resp.Diagnostics.HasError() {
		return
	}

	// Config wins over the environment, so a workspace can pin one instance
	// while the shell holds the default.
	endpoint := os.Getenv(envEndpoint)
	if !config.Endpoint.IsNull() && config.Endpoint.ValueString() != "" {
		endpoint = config.Endpoint.ValueString()
	}
	apiKey := os.Getenv(envAPIKey)
	if !config.APIKey.IsNull() && config.APIKey.ValueString() != "" {
		apiKey = config.APIKey.ValueString()
	}

	if apiKey == "" {
		resp.Diagnostics.AddAttributeError(
			path.Root("api_key"),
			"Missing LumaTrack API key",
			"Set api_key in the provider block, or export "+envAPIKey+". Create the key in LumaTrack under "+
				"Settings, API keys; an ingest-only key is enough to record runs.",
		)
		return
	}

	client := NewClient(endpoint, apiKey)
	resp.ResourceData = client
	resp.DataSourceData = client
}

func (p *lumatrackProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewRunResource,
	}
}

// No data sources yet. Reading the ledger back needs a full-access key, and
// this provider is built to run with an ingest-only one.
func (p *lumatrackProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return nil
}

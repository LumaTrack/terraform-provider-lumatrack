package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// The statuses the ingest contract accepts. 'skipped' is a run that started and
// deliberately did no work; 'cancelled' is one interrupted before it finished.
// Neither earns value and neither counts as a failure.
var runStatuses = []string{"success", "failure", "skipped", "cancelled"}

var (
	_ resource.Resource              = &runResource{}
	_ resource.ResourceWithConfigure = &runResource{}
)

func NewRunResource() resource.Resource {
	return &runResource{}
}

type runResource struct {
	client *Client
}

type runModel struct {
	ID              types.String `tfsdk:"id"`
	Automation      types.String `tfsdk:"automation"`
	Status          types.String `tfsdk:"status"`
	DurationSeconds types.Int64  `tfsdk:"duration_seconds"`
	Units           types.Int64  `tfsdk:"units"`
	ExternalID      types.String `tfsdk:"external_id"`
	Source          types.String `tfsdk:"source"`
	FailureReason   types.String `tfsdk:"failure_reason"`
	ExecutedAt      types.String `tfsdk:"executed_at"`
	Metadata        types.Map    `tfsdk:"metadata"`
	Held            types.Bool   `tfsdk:"held"`
	Deduplicated    types.Bool   `tfsdk:"deduplicated"`
}

func (r *runResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_run"
}

func (r *runResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	// Every configurable attribute forces replacement: a recorded run is an
	// immutable fact, and the ingest API has no update. Changing what this
	// resource reports means a different run happened, so a new event is
	// recorded rather than the old one edited.
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reports one automation run event to the LumaTrack value ledger when applied. " +
			"A run is an immutable fact: every change to this resource replaces it and records a new event, " +
			"and destroying it removes it from Terraform state without deleting the recorded evidence.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The LumaTrack run id (`run_...`).",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"automation": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Slug of the automation this run reports as. List them with " +
					"`GET /api/v1/automations`, or create them in the app.",
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"status": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString("success"),
				MarkdownDescription: "One of `success`, `failure`, `skipped`, `cancelled`. Defaults to " +
					"`success`. Report failures too: they cost money and save nothing, and the ledger " +
					"prices that honestly.",
				Validators: []validator.String{
					stringvalidator.OneOf(runStatuses...),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"duration_seconds": schema.Int64Attribute{
				Optional:            true,
				MarkdownDescription: "Wall-clock runtime in seconds. Omit it rather than sending a guess.",
				Validators: []validator.Int64{
					int64validator.AtLeast(0),
				},
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.RequiresReplace(),
				},
			},
			"units": schema.Int64Attribute{
				Optional: true,
				MarkdownDescription: "Records or items this run processed. Multiplies value when the " +
					"automation is valued per unit. The server defaults to 1 when omitted.",
				Validators: []validator.Int64{
					int64validator.AtLeast(0),
				},
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.RequiresReplace(),
				},
			},
			"external_id": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Your system's run id, which makes ingestion idempotent. When you leave " +
					"it unset the provider generates one and keeps it in state, so re-applying unchanged " +
					"config records nothing new. Set it to your pipeline's own run id when you want " +
					"exactly-once reporting to survive a lost state file.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"source": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString("terraform"),
				MarkdownDescription: "Reporting system stored on the run. Defaults to `terraform`; " +
					"override it to tell environments or pipelines apart.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"failure_reason": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Root cause for a failed run, e.g. `auth/credential`. Powers the " +
					"failure-reason Pareto on the automation page.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"executed_at": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "ISO 8601 timestamp, for evidence that genuinely happened earlier. " +
					"Omit it for live events. Closed months are refused.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"metadata": schema.MapAttribute{
				Optional:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "String key/value pairs kept with the run.",
				PlanModifiers: []planmodifier.Map{
					mapplanmodifier.RequiresReplace(),
				},
			},
			"held": schema.BoolAttribute{
				Computed: true,
				MarkdownDescription: "True when the run was stored but held over the plan's monthly event " +
					"cap. Held runs are excluded from every value number until the plan is raised.",
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.UseStateForUnknown(),
				},
			},
			"deduplicated": schema.BoolAttribute{
				Computed: true,
				MarkdownDescription: "True when this `external_id` had already been recorded, so the " +
					"request was an idempotent replay rather than a new run.",
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.UseStateForUnknown(),
				},
			},
		},
	}
}

func (r *runResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected provider data",
			fmt.Sprintf("Expected *provider.Client, got %T. This is a bug in the provider.", req.ProviderData),
		)
		return
	}
	r.client = client
}

func (r *runResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan runModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	externalID := plan.ExternalID.ValueString()
	if plan.ExternalID.IsNull() || plan.ExternalID.IsUnknown() || externalID == "" {
		generated, err := generateExternalID()
		if err != nil {
			resp.Diagnostics.AddError("Could not generate an external id", err.Error())
			return
		}
		externalID = generated
	}

	run := RunRequest{
		Automation:    plan.Automation.ValueString(),
		Status:        plan.Status.ValueString(),
		ExternalID:    externalID,
		Source:        plan.Source.ValueString(),
		FailureReason: plan.FailureReason.ValueString(),
		ExecutedAt:    plan.ExecutedAt.ValueString(),
	}
	if !plan.DurationSeconds.IsNull() && !plan.DurationSeconds.IsUnknown() {
		duration := plan.DurationSeconds.ValueInt64()
		run.DurationSeconds = &duration
	}
	if !plan.Units.IsNull() && !plan.Units.IsUnknown() {
		units := plan.Units.ValueInt64()
		run.Units = &units
	}
	if !plan.Metadata.IsNull() && !plan.Metadata.IsUnknown() {
		metadata := map[string]string{}
		resp.Diagnostics.Append(plan.Metadata.ElementsAs(ctx, &metadata, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
		run.Metadata = metadata
	}

	recorded, err := r.client.RecordRun(ctx, run)
	if err != nil {
		resp.Diagnostics.AddError("Could not record the run in LumaTrack", err.Error())
		return
	}

	// ignored_fields means the server did not recognize part of the payload and
	// recorded the run without it. Silence here is how a misspelled field
	// quietly stops booking cost, so it surfaces as a warning.
	if len(recorded.IgnoredFields) > 0 {
		resp.Diagnostics.AddWarning(
			"LumaTrack ignored part of this run",
			"The server did not recognize: "+strings.Join(recorded.IgnoredFields, ", ")+
				". The run was recorded without those fields.",
		)
	}
	if recorded.Warning != "" {
		resp.Diagnostics.AddWarning("LumaTrack returned a warning", recorded.Warning)
	}
	if recorded.Run.Held {
		resp.Diagnostics.AddWarning(
			"This run is held over the plan cap",
			"LumaTrack stored the run but excluded it from every value number, because the organization "+
				"has passed its monthly event allowance. Raise the plan to release held runs.",
		)
	}

	plan.ID = types.StringValue(recorded.Run.ID)
	plan.ExternalID = types.StringValue(externalID)
	plan.Held = types.BoolValue(recorded.Run.Held)
	plan.Deduplicated = types.BoolValue(recorded.Deduplicated)

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read keeps state as it is. A recorded run is append-only evidence, and the
// ingest-only key this provider is built for cannot read runs back at all: the
// API acknowledges a replay rather than echoing it. Refreshing would therefore
// either do nothing or need a full-access key for no benefit.
func (r *runResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state runModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is unreachable: every configurable attribute forces replacement. It
// stays honest rather than empty so a schema change that accidentally makes an
// attribute updatable fails loudly instead of silently editing history.
func (r *runResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError(
		"A recorded run cannot be changed",
		"Runs are append-only evidence, so this resource replaces itself on any change. Reaching Update "+
			"means the provider's schema is wrong; please report it.",
	)
}

// Delete drops the resource from Terraform state and calls nothing. The run
// happened, so the ledger keeps it: destroying the Terraform object must not
// rewrite history. Removing evidence is a role-gated human decision in the app.
func (r *runResource) Delete(_ context.Context, _ resource.DeleteRequest, resp *resource.DeleteResponse) {
	resp.Diagnostics.AddWarning(
		"The recorded run stays in LumaTrack",
		"Terraform forgot this run, and the ledger did not. Run evidence is append-only; delete it in the "+
			"app if it was recorded in error.",
	)
}

// generateExternalID makes a random idempotency key. It is stored in state, so
// a re-apply of unchanged config reports nothing new.
func generateExternalID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("could not read random bytes: %w", err)
	}
	return "tf-" + hex.EncodeToString(buf), nil
}

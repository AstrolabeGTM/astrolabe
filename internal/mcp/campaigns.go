package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AstrolabeGTM/astrolabe/internal/campaign"
	"github.com/AstrolabeGTM/astrolabe/internal/product"
	"github.com/AstrolabeGTM/astrolabe/internal/render"
)

// Sales words for the campaign tools, merged into the shared table.
func init() {
	for name, aka := range map[string]string{
		"campaigns":           "ad campaigns, flights, insertion orders, media plan, launch plan",
		"campaign":            "campaign brief, flight details, pre-flight checklist status, insertion order",
		"set_campaign":        "change the budget, flight dates, pacing or stop-loss; insertion order change",
		"campaign_history":    "budget change log, audit trail, who changed the flight",
		"check_gate":          "pre-flight checklist, launch readiness, go/no-go, approval to spend",
		"record_spend":        "log ad spend, add a cost line, actuals",
		"import_spend":        "upload a platform spend report, import ad platform CSV, clicks and cost export",
		"creatives":           "ad creative, creative review, asset library, proofs",
		"render_creative":     "produce ad creative at each size, resize, generate banner or social images",
		"edit_creative":       "change ad copy, creative revision",
		"approve_creative":    "creative review sign-off, approve ad copy and assets",
		"export_pack":         "creative handoff, asset package, media kit for posting",
		"make_campaign_tasks": "launch plan tasks, posting schedule, content calendar",
		"campaign_report":     "campaign performance, ad report, spend vs results, ROI, cost per acquisition",
		"campaigns_json":      "landing page variants, per-campaign landing pages, UTM landing list",
	} {
		salesTerms[name] = aka
	}
}

func (s *Server) registerCampaigns(srv *sdk.Server) {
	register(srv, "campaigns", "List campaigns (a launch week, a small paid test) with dates, spend against the cap, gate status (how many pre-spend checks are done) and warnings. Omit product for all; running_only limits to campaigns that have started and not ended.", s.campaigns)
	register(srv, "campaign", "One campaign in full: goal, audience, offer, landing page, effective budget cap, dates and stop rule, gate items checked or not, spend so far, warnings, launch tasks, creatives with approval status, and recent changes. campaign is the file id or short code.", s.campaign)
	register(srv, "set_campaign", "Change a campaign's budget cap (a number such as 200 or 200.50), currency, start or end date (YYYY-MM-DD; end \"\" removes it), stop rule text, or the checkable stop rule (stop_spend_pct and stop_max_events: stop when that percent of the cap is spent with at most that many success events), or clear_stop_rule. Every change is kept in the history with who, when, old and new value. Spend is never raised by anything but this call, and only when you ask.", s.setCampaign)
	register(srv, "campaign_history", "Every change to a campaign's budget, dates, stop rule and gate checks: who, when, old and new value.", s.campaignHistory)
	register(srv, "check_gate", "Check (or uncheck) one pre-spend gate item by its text or number. The gate blocks nothing: spend recorded before it is complete is allowed but flagged, permanently, on the entry.", s.checkGate)
	register(srv, "record_spend", "Record spend by hand: amount (e.g. 25 or 25.50, in the campaign's currency), platform, optional date (YYYY-MM-DD, default today), note, and the platform's clicks and impressions if you have them. Returns warnings: near the cap (80%), over the cap, spent before the gate was complete, stop rule met. Nothing is stopped or changed by a warning.", s.recordSpend)
	register(srv, "import_spend", "Import a platform's spend and clicks CSV (header row with date, spend/cost/amount and optionally clicks, impressions, platform). Rows for the same platform and day are added together; importing the same file again replaces those days rather than adding to them. Unreadable rows are skipped and listed.", s.importSpend)
	register(srv, "creatives", "A campaign's creatives (HTML template plus copy fields): copy, alt text, claims-check flags, rendered files and whether the approval still holds.", s.creatives)
	register(srv, "render_creative", "Render a creative to PNG (and PDF if asked) at each required size through the optional Chromium sidecar (docker compose --profile creative up, ASTROLABE_RENDER_URL). Without it this says plainly what to start; copy, flags and approval still work. Rendering withdraws any approval, since the files are new.", s.renderCreative)
	register(srv, "edit_creative", "Change a creative's copy fields and/or alt text. Any real edit withdraws the approval and drops the rendered files. The claims check runs on the new copy (never-claims, placeholders, numbers without a source); flagged copy cannot be approved.", s.editCreative)
	register(srv, "approve_creative", "Approve a creative exactly as it is now: the copy, the template and every rendered file, by hash. Only on the operator's say-so. Refused while flagged. Any later edit withdraws it.", s.approveCreative)
	register(srv, "export_pack", "Write an approved pack to a folder for posting by hand: rendered files, copy, alt text, the posts and the landing page URL. Refuses if any creative is unapproved or any post is flagged. Nothing is posted and no ad platform is contacted. dir defaults to exports/<campaign> in the product folder.", s.exportPack)
	register(srv, "make_campaign_tasks", "Turn the campaign's launch tasks into tasks and content items (due on start date + day offset), each with the post text and a tracked link. Mark one posted with content_mark and its URL. Safe to run again; posts flagged by the claims check are held back and listed.", s.makeCampaignTasks)
	register(srv, "campaign_report", "Campaign report for its window: spend and the cap, the platform's clicks as entered, success events attributed to the campaign (signals or funnel events carrying campaign=<code>), each count with what it is out of, and what is not measured. Zero shows as zero; no rates, no cost per event without events.", s.campaignReport)
	register(srv, "campaigns_json", "The campaigns.json export for a website to prerender per-campaign landing pages: a JSON array of {code,label} for campaigns that have not ended. Returns the exact file text. (CLI: astrolabe campaigns export.)", s.campaignsJSON)
}

type campaignArg struct {
	Product  string `json:"product" jsonschema:"product id"`
	Campaign string `json:"campaign" jsonschema:"campaign file id or short code"`
}

type campaignsArg struct {
	Product     string `json:"product,omitempty" jsonschema:"product id; omit for all products"`
	RunningOnly bool   `json:"running_only,omitempty"`
}

type setCampaignArg struct {
	campaignArg
	BudgetCap     *float64 `json:"budget_cap,omitempty" jsonschema:"new cap in the campaign's currency, e.g. 200 or 200.5; 0 means nothing planned"`
	Currency      *string  `json:"currency,omitempty" jsonschema:"3-letter code; refused once spend is recorded"`
	Start         *string  `json:"start,omitempty" jsonschema:"YYYY-MM-DD"`
	End           *string  `json:"end,omitempty" jsonschema:"YYYY-MM-DD; empty string removes the end date"`
	StopRuleText  *string  `json:"stop_rule_text,omitempty"`
	StopSpendPct  *int     `json:"stop_spend_pct,omitempty" jsonschema:"checkable stop rule: percent of the cap spent (1-100)"`
	StopMaxEvents *int     `json:"stop_max_events,omitempty" jsonschema:"... with at most this many success events (default 0)"`
	ClearStopRule bool     `json:"clear_stop_rule,omitempty" jsonschema:"remove the checkable stop rule"`
	Note          string   `json:"note,omitempty" jsonschema:"why, kept in the history"`
}

type gateArg struct {
	campaignArg
	Item    string `json:"item" jsonschema:"gate item text or its number (from campaign)"`
	Checked *bool  `json:"checked,omitempty" jsonschema:"default true; false unchecks"`
	Note    string `json:"note,omitempty"`
}

type spendArg struct {
	campaignArg
	Amount      float64 `json:"amount" jsonschema:"in the campaign's currency, at most 2 decimals"`
	Platform    string  `json:"platform" jsonschema:"where it was spent, e.g. linkedin, reddit, newsletter"`
	Date        string  `json:"date,omitempty" jsonschema:"YYYY-MM-DD, default today"`
	Note        string  `json:"note,omitempty"`
	Clicks      *int64  `json:"clicks,omitempty" jsonschema:"the platform's own click count, as it reports it"`
	Impressions *int64  `json:"impressions,omitempty"`
}

type importSpendArg struct {
	campaignArg
	CSV      string `json:"csv" jsonschema:"CSV text including the header row"`
	Platform string `json:"platform,omitempty" jsonschema:"platform for every row, if the CSV has no platform column"`
}

type creativeArg struct {
	campaignArg
	Creative string `json:"creative,omitempty" jsonschema:"creative id; omit to render all of the campaign's creatives (render_creative)"`
}

type editCreativeArg struct {
	campaignArg
	Creative string            `json:"creative"`
	Copy     map[string]string `json:"copy,omitempty" jsonschema:"copy fields to change, by name"`
	Alt      *string           `json:"alt,omitempty"`
}

type exportArg struct {
	campaignArg
	Dir string `json:"dir,omitempty" jsonschema:"folder to write to; default exports/<campaign> inside the product folder"`
}

const by = "mcp"

func (s *Server) campaignTarget(a campaignArg) (*product.Product, *product.Campaign, error) {
	if s.S == nil || s.S.Pool == nil {
		return nil, nil, errors.New("campaigns need the database")
	}
	p, err := s.find(a.Product)
	if err != nil {
		return nil, nil, err
	}
	c := p.Campaign(a.Campaign)
	if c == nil {
		var ids []string
		for _, x := range p.Campaigns {
			ids = append(ids, x.ID+" ("+x.Code+")")
		}
		return nil, nil, fmt.Errorf("%s has no campaign %q (have %v). Campaigns are files in products/%s/campaigns/", p.ID, a.Campaign, ids, p.ID)
	}
	return p, c, nil
}

func (s *Server) renderer() *render.Client { return s.Render }

func (s *Server) campaigns(ctx context.Context, a campaignsArg) (any, error) {
	ps, err := s.productsFor(a.Product)
	if err != nil {
		return nil, err
	}
	out := []campaign.Summary{}
	for _, p := range ps {
		sm, err := campaign.Summaries(ctx, s.S.Pool, p, time.Now(), a.RunningOnly)
		if err != nil {
			return nil, err
		}
		out = append(out, sm...)
	}
	return map[string]any{"campaigns": out}, nil
}

func (s *Server) campaign(ctx context.Context, a campaignArg) (any, error) {
	p, c, err := s.campaignTarget(a)
	if err != nil {
		return nil, err
	}
	st, err := campaign.Check(ctx, s.S.Pool, p, c, time.Now())
	if err != nil {
		return nil, err
	}
	creatives, err := campaign.Creatives(ctx, s.S.Pool, p, c)
	if err != nil {
		return nil, err
	}
	posts, held, err := campaign.Posts(ctx, s.S.Pool, p, c)
	if err != nil {
		return nil, err
	}
	spend, err := campaign.SpendList(ctx, s.S.Pool, p, c, 20)
	if err != nil {
		return nil, err
	}
	hist, err := campaign.History(ctx, s.S.Pool, p, c)
	if err != nil {
		return nil, err
	}
	if len(hist) > 10 {
		hist = hist[len(hist)-10:]
	}
	return map[string]any{
		"id": c.ID, "code": c.Code, "label": c.Label, "goal": c.Goal, "audience": c.Audience, "channels": c.Channels,
		"offer": c.Offer, "landing": c.Landing, "success_event": c.SuccessEvent,
		"status": st, "stop_rule": st.Settings.StopDescription(),
		"posts": posts, "posts_held_by_claims_check": held, "creatives": creatives,
		"recent_spend": spend, "recent_changes": hist,
		"renderer": map[string]any{"configured": s.renderer().Enabled(), "how_to_enable": render.HowToEnable},
	}, nil
}

func (s *Server) setCampaign(ctx context.Context, a setCampaignArg) (any, error) {
	p, c, err := s.campaignTarget(a.campaignArg)
	if err != nil {
		return nil, err
	}
	ch := campaign.Change{Currency: a.Currency, Start: a.Start, End: a.End, StopText: a.StopRuleText,
		StopSpendPct: a.StopSpendPct, StopMaxEvents: a.StopMaxEvents, ClearStopRule: a.ClearStopRule, Note: a.Note}
	if a.BudgetCap != nil {
		m, err := product.ParseMoney(strconv.FormatFloat(*a.BudgetCap, 'f', -1, 64))
		if err != nil {
			return nil, err
		}
		ch.BudgetCap = &m
	}
	changes, err := campaign.Set(ctx, s.S.Pool, p, c, by, ch)
	if err != nil {
		return nil, err
	}
	st, err := campaign.Check(ctx, s.S.Pool, p, c, time.Now())
	if err != nil {
		return nil, err
	}
	return map[string]any{"changed": changes, "settings": st.Settings, "warnings": st.Warnings}, nil
}

func (s *Server) campaignHistory(ctx context.Context, a campaignArg) (any, error) {
	p, c, err := s.campaignTarget(a)
	if err != nil {
		return nil, err
	}
	h, err := campaign.History(ctx, s.S.Pool, p, c)
	return map[string]any{"history": h}, err
}

func (s *Server) checkGate(ctx context.Context, a gateArg) (any, error) {
	p, c, err := s.campaignTarget(a.campaignArg)
	if err != nil {
		return nil, err
	}
	checked := a.Checked == nil || *a.Checked
	item, err := campaign.CheckGate(ctx, s.S.Pool, p, c, a.Item, checked, by, a.Note)
	if err != nil {
		return nil, err
	}
	items, ok, err := campaign.Gate(ctx, s.S.Pool, p, c)
	return map[string]any{"item": item, "gate": items, "gate_complete": ok}, err
}

func (s *Server) recordSpend(ctx context.Context, a spendArg) (any, error) {
	p, c, err := s.campaignTarget(a.campaignArg)
	if err != nil {
		return nil, err
	}
	m, err := product.ParseMoney(strconv.FormatFloat(a.Amount, 'f', -1, 64))
	if err != nil {
		return nil, err
	}
	if a.Date == "" {
		a.Date = time.Now().Format("2006-01-02")
	}
	return campaign.RecordSpend(ctx, s.S.Pool, p, c, by, campaign.SpendInput{Date: a.Date, Amount: m, Platform: a.Platform, Note: a.Note,
		Clicks: a.Clicks, Impressions: a.Impressions}, time.Now())
}

func (s *Server) importSpend(ctx context.Context, a importSpendArg) (any, error) {
	p, c, err := s.campaignTarget(a.campaignArg)
	if err != nil {
		return nil, err
	}
	return campaign.ImportSpend(ctx, s.S.Pool, p, c, by, a.Platform, strings.NewReader(a.CSV), time.Now())
}

func (s *Server) creatives(ctx context.Context, a campaignArg) (any, error) {
	p, c, err := s.campaignTarget(a)
	if err != nil {
		return nil, err
	}
	v, err := campaign.Creatives(ctx, s.S.Pool, p, c)
	return map[string]any{"creatives": v, "renderer_configured": s.renderer().Enabled()}, err
}

func (s *Server) creativeTarget(a campaignArg, id string) (*product.Product, *product.Campaign, *product.Creative, error) {
	p, c, err := s.campaignTarget(a)
	if err != nil {
		return nil, nil, nil, err
	}
	cr := c.Creative(id)
	if cr == nil {
		var ids []string
		for _, x := range c.Creatives {
			ids = append(ids, x.ID)
		}
		return nil, nil, nil, fmt.Errorf("campaign %s has no creative %q (have %v)", c.ID, id, ids)
	}
	return p, c, cr, nil
}

func (s *Server) renderCreative(ctx context.Context, a creativeArg) (any, error) {
	p, c, err := s.campaignTarget(a.campaignArg)
	if err != nil {
		return nil, err
	}
	if !s.renderer().Enabled() {
		return nil, render.ErrNoRenderer
	}
	var out []*campaign.CreativeView
	for _, cr := range c.Creatives {
		if a.Creative != "" && cr.ID != a.Creative {
			continue
		}
		v, err := campaign.RenderCreative(ctx, s.S.Pool, s.renderer(), p, c, cr, by)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("campaign %s has no creative %q", c.ID, a.Creative)
	}
	return map[string]any{"creatives": out, "note": "Rendering withdraws any earlier approval; approve again after looking at the files."}, nil
}

func (s *Server) editCreative(ctx context.Context, a editCreativeArg) (any, error) {
	p, c, cr, err := s.creativeTarget(a.campaignArg, a.Creative)
	if err != nil {
		return nil, err
	}
	return campaign.EditCreative(ctx, s.S.Pool, p, c, cr, by, a.Copy, a.Alt)
}

func (s *Server) approveCreative(ctx context.Context, a editCreativeArg) (any, error) {
	p, c, cr, err := s.creativeTarget(a.campaignArg, a.Creative)
	if err != nil {
		return nil, err
	}
	return campaign.ApproveCreative(ctx, s.S.Pool, p, c, cr, by)
}

func (s *Server) exportPack(ctx context.Context, a exportArg) (any, error) {
	p, c, err := s.campaignTarget(a.campaignArg)
	if err != nil {
		return nil, err
	}
	dir := a.Dir
	switch {
	case dir == "":
		if p.Dir == "" {
			return nil, errors.New("this product has no folder on disk; pass dir")
		}
		dir = filepath.Join(p.Dir, "exports", c.ID)
	case !filepath.IsAbs(dir) && p.Dir != "":
		if strings.Contains(dir, "..") {
			return nil, errors.New("dir must not contain ..")
		}
		dir = filepath.Join(p.Dir, dir)
	}
	return campaign.ExportPack(ctx, s.S.Pool, p, c, dir, by)
}

func (s *Server) makeCampaignTasks(ctx context.Context, a campaignArg) (any, error) {
	p, c, err := s.campaignTarget(a)
	if err != nil {
		return nil, err
	}
	return campaign.CreateTasks(ctx, s.S.Pool, s.S, s.S.PublicURL, p, c, time.Now())
}

func (s *Server) campaignReport(ctx context.Context, a campaignArg) (any, error) {
	p, c, err := s.campaignTarget(a)
	if err != nil {
		return nil, err
	}
	r, err := campaign.BuildReport(ctx, s.S.Pool, p, c, time.Now())
	if err != nil {
		return nil, err
	}
	return map[string]any{"text": r.Text(), "data": r}, nil
}

func (s *Server) campaignsJSON(ctx context.Context, a productOpt) (any, error) {
	if s.S == nil || s.S.Pool == nil {
		return nil, errors.New("campaigns need the database")
	}
	ps, err := s.productsFor(a.Product)
	if err != nil {
		return nil, err
	}
	v, err := campaign.Variants(ctx, s.S.Pool, ps, time.Now())
	if err != nil {
		return nil, err
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	return map[string]any{"campaigns": v, "file": string(b) + "\n"}, nil
}

package forget

// Handled is the registry the guard test checks the schema against: every
// column that is a foreign key to people(id), or is named person_id, must be
// listed here with what Forget does with it. A table added later that refers
// to a person fails the test until it is handled in run.go and listed here.
// (Foreign keys also make a missed table fail closed: deleting the person
// row is refused, the transaction rolls back and nothing is erased.)
var Handled = map[string]string{
	"identities.person_id":          "deleted",
	"product_people.person_id":      "deleted",
	"suppressions.person_id":        "deleted",
	"enrollments.person_id":         "deleted",
	"actions.person_id":             "deleted (a cold send inside the 24-hour cap leaves an anonymous erased_sends row)",
	"replies.person_id":             "deleted",
	"tasks.person_id":               "deleted",
	"funnel_events.person_id":       "deleted",
	"signals.person_id":             "deleted",
	"identity_reviews.resolved_to":  "deleted (with reviews listing them in candidates)",
	"scores.person_id":              "deleted",
	"briefs.person_id":              "deleted (with the LLM call behind it)",
	"play_runs.person_id":           "deleted; scrubbed instead inside a play's 24-hour limit",
	"payments.person_id":            "deleted",
	"channel_permissions.person_id": "deleted",
	"links.person_id":               "deleted (with their clicks)",
	"account_people.person_id":      "deleted (the account and company stay)",
	"form_submissions.person_id":    "deleted (and at Netlify first)",
	"conversations.person_id":       "deleted",
	"pmf_answers.person_id":         "deleted",
	"pricing_feedback.person_id":    "deleted",
	"updates_subscribers.person_id": "deleted (with their messages)",
}

// Indirect lists personal data that has no person column, and how Forget
// reaches it. The guard test cannot see these; add to the list when adding
// such a table.
var Indirect = map[string]string{
	"updates_messages":       "by subscriber_id",
	"clicks":                 "by the code of their links",
	"sandbox_outbox":         "by action_id, and by recipient address",
	"voice_examples":         "by action_id",
	"assignments":            "unit = 'person:<id>'",
	"identity_reviews":       "candidates array contains their id",
	"llm_calls":              "the brief's call, and any call whose text names them",
	"river_job":              "jobs whose args name their id, an action, reply or task of theirs",
	"signals":                "also by subject email or GitHub login, for signals not matched to a person",
	"form_submissions":       "also by the address inside fields, for submissions not matched to a person",
	"replies":                "also by from_addr",
	"payments":               "also by email",
	"weekly_reviews":         "no person column; strings in snapshot, answers and decisions that name them are replaced (jsonScrubs in run.go)",
	"accounts and companies": "free text naming them is overwritten (see scrubs in run.go); the rows stay",
}

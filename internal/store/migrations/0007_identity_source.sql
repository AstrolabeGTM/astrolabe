-- Where each identity came from. The outreach guard needs it: GitHub's
-- Acceptable Use Policies (section 7) forbid emailing people on the strength
-- of information taken from GitHub, so an email address found through GitHub
-- (monitor, profile enrichment, commit) must stay distinguishable from one the
-- person gave us (a form or product event, a reply, an import, a manual entry).
--
-- Values: github, event (product events and tracked links), stripe, reply,
-- import, manual, form.
ALTER TABLE identities ADD COLUMN source text NOT NULL DEFAULT 'manual';

-- Backfill. Non-email identities: the GitHub login is 'github', the rest
-- came in with a signal ('event').
UPDATE identities SET source = CASE WHEN kind = 'github' THEN 'github' ELSE 'event' END
WHERE kind <> 'email';

-- Email identities, first match wins:
--  1. the person wrote to us from that address                       -> reply
--  2. a signal for that person carried that address as its subject   -> stripe / manual / event
--  3. the person came in through an import                           -> import
--  4. the person has a GitHub login (so the address was almost
--     certainly the public profile email found by enrichment)        -> github
--  5. anything else, which only the operator can have entered        -> manual
UPDATE identities i SET source = CASE
    WHEN EXISTS (SELECT 1 FROM replies r WHERE r.person_id = i.person_id AND r.from_addr = i.value AND NOT r.is_bounce)
        THEN 'reply'
    WHEN EXISTS (SELECT 1 FROM signals s WHERE s.person_id = i.person_id AND lower(s.subject->>'email') = i.value AND s.type LIKE 'stripe.%')
        THEN 'stripe'
    WHEN EXISTS (SELECT 1 FROM signals s WHERE s.person_id = i.person_id AND lower(s.subject->>'email') = i.value AND s.type LIKE 'manual.%')
        THEN 'manual'
    WHEN EXISTS (SELECT 1 FROM signals s WHERE s.person_id = i.person_id AND lower(s.subject->>'email') = i.value AND s.type NOT LIKE 'link.%')
        THEN 'event'
    WHEN EXISTS (SELECT 1 FROM product_people pp WHERE pp.person_id = i.person_id AND pp.source = 'import')
        THEN 'import'
    WHEN EXISTS (SELECT 1 FROM identities g WHERE g.person_id = i.person_id AND g.kind = 'github')
        THEN 'github'
    ELSE 'manual'
END
WHERE i.kind = 'email';

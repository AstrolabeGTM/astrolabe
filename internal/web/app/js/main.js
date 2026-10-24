// Entry point: register the screens, then start the app.
import { register, start } from "./app.js";
import today from "./today.js";
import people from "./people.js";
import person from "./person.js";
import signals from "./signals.js";
import reception from "./reception.js";
import settings from "./settings.js";
import pipeline from "./pipeline.js";
import account from "./account.js";
import { campaignsList, campaignDetail } from "./campaigns.js";
import review from "./review.js";
import updates from "./updates.js";
import calendar from "./calendar.js";
import { funnel, automations, outbox, digest } from "./insights.js";

register(today, pipeline, account, people, person, signals, campaignsList, campaignDetail, calendar, updates, outbox,
  reception, funnel, review, automations, digest, settings);
start();

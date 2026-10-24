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

register(today, pipeline, account, people, person, signals, reception, settings);
start();

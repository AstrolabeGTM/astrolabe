// Entry point: register the screens, then start the app.
import { register, start } from "./app.js";
import today from "./today.js";

register(today);
start();

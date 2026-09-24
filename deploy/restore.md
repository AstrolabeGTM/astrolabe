# Restore a backup

Test this once after the first backup exists.

```sh
gcloud compute ssh astrolabe --tunnel-through-iap
sudo systemctl stop astrolabe
gcloud storage cp gs://<project>-astrolabe-backups/astrolabe-YYYY-MM-DD.dump /tmp/a.dump
sudo -u postgres dropdb astrolabe && sudo -u postgres createdb -O astrolabe astrolabe
sudo -u postgres pg_restore -d astrolabe /tmp/a.dump
sudo systemctl start astrolabe
```

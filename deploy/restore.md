# Restore a backup

Test this once after the first backup exists.

```sh
gcloud compute ssh astrolabe --tunnel-through-iap
cd /opt/astrolabe
sudo docker compose stop astrolabe
gcloud storage cp gs://<project>-astrolabe-backups/astrolabe-YYYY-MM-DD.dump /tmp/a.dump
sudo docker compose exec -T postgres dropdb -U astrolabe astrolabe
sudo docker compose exec -T postgres createdb -U astrolabe astrolabe
sudo docker compose exec -T postgres pg_restore -U astrolabe -d astrolabe < /tmp/a.dump
sudo docker compose start astrolabe
```

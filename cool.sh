#!/usr/bin/env bash
# if test "$BASH" = "" || "$BASH" -uc "a=();true \"\${a[@]}\"" 2>/dev/null; then
#     # Bash 4.4, Zsh
#     set -euo pipefail
# else
#     # Bash 4.3 and older chokes on empty arrays with set -u.
#     set -eo pipefail
# fi
# shopt -s nullglob globstar


rm -rf tyk tyk.conf
rm -rf my_first_api.json upload_api_with_middleware.json

docker ps -aq | xargs docker rm -f
docker volume ls -q | xargs docker volume rm -f
fuser -k 7391/tcp
fuser -k 3121/tcp

echo '{
  "listen_address": "localhost",
  "listen_port": 7391,
  "secret": "changeMe",
  "template_path": "./templates",
  "enable_http_profiler": true,
  "storage": {
    "type": "redis",
    "host": "localhost",
    "port": 6379,
    "username": "",
    "password": "",
    "database": 0
  }
}' >> tyk.conf

prev_dir=$(pwd)
cd my_app && go build -o my_app && ./my_app &
cd "$prev_dir"

docker run -d --rm -p 6379:6379/tcp redis --loglevel verbose

go build -gcflags="all=-N -l" -o tyk

./tyk start --conf=tyk.conf


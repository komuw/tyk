#!/usr/bin/env bash
# if test "$BASH" = "" || "$BASH" -uc "a=();true \"\${a[@]}\"" 2>/dev/null; then
#     # Bash 4.4, Zsh
#     set -euo pipefail
# else
#     # Bash 4.3 and older chokes on empty arrays with set -u.
#     set -eo pipefail
# fi
# shopt -s nullglob globstar

printf "\n\n\t check tyk-gateway is up. \n"
curl http://localhost:7391/hello


printf "\n\n\t create API. \n"
curl -v \
  -H "x-tyk-authorization: changeMe" \
  -H "Content-Type: application/json" \
  -X POST \
  -d '{
    "name": "upload_api_with_middleware",
    "slug": "upload_api_with_middleware",
    "api_id": "upload_api_with_middleware",
    "use_keyless": true,
    "version_data": {
        "not_versioned": true,
        "versions": {
            "Default": {
                "name": "Default"
            }
        }
    },
    "proxy": {
        "listen_path": "/upload_api_with_middleware",
        "target_url": "http://localhost:3121",
        "strip_listen_path": true
    },
    "active": true
}' http://localhost:7391/tyk/apis

# curl -vkL -X POST --data-binary "@my_app/test-5mb.bin" http://localhost:7391/upload_api_with_middleware/upload



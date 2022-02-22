docker ps -aq | xargs docker rm -f
docker run --rm -p 6379:6379/tcp redis --loglevel verbose

cd /home/komuw/paidWork/tyk && \
rm -rf tyk && \
go1.15.2 build -o tyk && \
cp tyk ~/Downloads/cool && \
cp templates/* ~/Downloads/cool/templates && \
cd /home/komuw/Downloads/cool && \
./tyk start --conf=my_tyk.conf

curl -v \
  -H "x-tyk-authorization: changeMe" \
  -H "Content-Type: application/json" \
  -X POST \
  -d '{
    "name": "my_first_api",
    "slug": "my_first_api",
    "api_id": "my_first_api",
    "use_keyless": true,
    "auth": {
      "auth_header_name": "X-example.com-API-KEY"
    },
    "version_data": {
      "not_versioned": true,
      "versions": {
        "Default": {
          "name": "Default",
          "use_extended_paths": true
        }
      }
    },
    "proxy": {
      "listen_path": "/my_first_api",
      "target_url": "http://httpbin.org/get",
      "strip_listen_path": true
   },
    "active": true
}' http://localhost:7391/tyk/apis


curl -H "x-tyk-authorization: changeMe" http://localhost:7391/tyk/reload/group && \
curl -vkL -H "x-tyk-authorization: changeMe" http://localhost:7391/tyk/apis


# curl tyk-gateway directly
curl -vkL http://localhost:7391/my_first_api
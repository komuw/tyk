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

printf "\n\n\t reload gateway. \n"
curl -H "x-tyk-authorization: changeMe" http://localhost:7391/tyk/reload/group


# curl -vkL -H "x-tyk-authorization: changeMe" http://localhost:7391/tyk/apis
# curl -vkL -H "x-tyk-authorization: changeMe" http://localhost:7391/tyk/apis | grep -i upload_api_with_middleware
# curl -v http://localhost:7391/upload_api_with_middleware/upload -F "file=@my_app/test-5mb.bin"

# https://jvns.ca/blog/2017/09/24/profiling-go-with-pprof/
#
#   -inuse_space      Display in-use memory size
#   -inuse_objects    Display in-use object counts
#   -alloc_space      Display allocated memory size
#   -alloc_objects    Display allocated object counts
#
# amount of memory being used == inuse metrics
# time spent in GC            == allocations metrics
#
# 1.
# curl -s http://localhost:7391/debug/pprof/heap > heap_before_upload.out
# go tool pprof tyk heap_before_upload.out
#    top 30
#    top -cum 30
#    list <regex>
#
# 2.
# curl -v http://localhost:7391/upload_api_with_middleware/upload -F "file=@my_app/test-5mb.bin"
# curl -s http://localhost:7391/debug/pprof/heap > heap_after_first_upload.out
# go tool pprof tyk heap_after_first_upload.out
# go tool pprof -base heap_before_upload.out tyk heap_after_first_upload.out
# go tool pprof -diff_base heap_before_upload.out tyk heap_after_first_upload.out
# go tool pprof -inuse_objects -base heap_before_upload.out tyk heap_after_first_upload.out
#
# 3.
# curl -v http://localhost:7391/upload_api_with_middleware/upload -F "file=@my_app/test-5mb.bin"
# curl -v http://localhost:7391/upload_api_with_middleware/upload -F "file=@my_app/test-5mb.bin"
# curl -v http://localhost:7391/upload_api_with_middleware/upload -F "file=@my_app/test-5mb.bin"
# curl -v http://localhost:7391/upload_api_with_middleware/upload -F "file=@my_app/test-5mb.bin"
# curl -v http://localhost:7391/upload_api_with_middleware/upload -F "file=@my_app/test-5mb.bin"
# curl -s http://localhost:7391/debug/pprof/heap > heap_after_sixth_upload.out
# go tool pprof tyk heap_after_sixth_upload.out
# go tool pprof -base heap_before_upload.out tyk heap_after_sixth_upload.out
#
# 4.
# curl -v http://localhost:7391/upload_api_with_middleware/upload -F "file=@my_app/test-5mb.bin"
# curl -v http://localhost:7391/upload_api_with_middleware/upload -F "file=@my_app/test-5mb.bin"
# curl -v http://localhost:7391/upload_api_with_middleware/upload -F "file=@my_app/test-5mb.bin"
# curl -v http://localhost:7391/upload_api_with_middleware/upload -F "file=@my_app/test-5mb.bin"
# curl -v http://localhost:7391/upload_api_with_middleware/upload -F "file=@my_app/test-5mb.bin"
# curl -s http://localhost:7391/debug/pprof/heap > heap_after_eleventh_upload.out
# go tool pprof tyk heap_after_eleventh_upload.out
# go tool pprof -base heap_before_upload.out tyk heap_after_eleventh_upload.out


# some notes about `io.Copy()`
# - if the reader has a `WriteTo` method it avoids an allocation and a copy.
# - if the writer has a `ReadFrom` method it is also faster.
# https://sourcegraph.com/github.com/golang/go@go1.15.15/-/blob/src/io/io.go?L385-396

#
# 100% 12817.54kB 92.49%  github.com/TykTechnologies/tyk/gateway.(*h2cWrapper).ServeHTTP
# 100% 12817.54kB 92.49%  github.com/TykTechnologies/tyk/gateway.(*handleWrapper).ServeHTTP
# 100% 12289.37kB 88.68%  github.com/TykTechnologies/tyk/gateway.copyBody
# 100% 12289.37kB 88.68%  github.com/TykTechnologies/tyk/gateway.copyRequest
# 100% 12289.37kB 88.68%  github.com/TykTechnologies/tyk/gateway.nopCloseRequestBody
#
# With the changes:
# (pprof) top -cum
# Showing nodes accounting for 5.01MB, 66.66% of 7.51MB total
# Showing top 10 nodes out of 32
#       flat  flat%   sum%        cum   cum%
#          0     0%     0%     5.01MB 66.66%  github.com/TykTechnologies/tyk/gateway.(*h2cWrapper).ServeHTTP
#          0     0%     0%     5.01MB 66.66%  github.com/TykTechnologies/tyk/gateway.(*handleWrapper).ServeHTTP
#     5.01MB 66.66% 66.66%     5.01MB 66.66%  github.com/TykTechnologies/tyk/gateway.copyBody
#          0     0% 66.66%     5.01MB 66.66%  github.com/TykTechnologies/tyk/gateway.copyRequest
#          0     0% 66.66%     5.01MB 66.66%  github.com/TykTechnologies/tyk/gateway.nopCloseRequestBody
#          0     0% 66.66%     5.01MB 66.66%  golang.org/x/net/http2/h2c.h2cHandler.ServeHTTP
#          0     0% 66.66%     5.01MB 66.66%  net/http.(*conn).serve
#          0     0% 66.66%     5.01MB 66.66%  net/http.serverHandler.ServeHTTP
#          0     0% 66.66%     1.50MB 19.97%  github.com/TykTechnologies/tyk/gateway.DoReload
#          0     0% 66.66%     1.50MB 19.97%  github.com/TykTechnologies/tyk/gateway.loadApps
# (pprof)
#
# Before the changes:
# (pprof) top -cum
# Showing nodes accounting for 12MB, 88.68% of 13.53MB total
# Showing top 10 nodes out of 32
#       flat  flat%   sum%        cum   cum%
#          0     0%     0%    12.52MB 92.49%  github.com/TykTechnologies/tyk/gateway.(*h2cWrapper).ServeHTTP
#          0     0%     0%    12.52MB 92.49%  github.com/TykTechnologies/tyk/gateway.(*handleWrapper).ServeHTTP
#          0     0%     0%    12.52MB 92.49%  golang.org/x/net/http2/h2c.h2cHandler.ServeHTTP
#          0     0%     0%    12.52MB 92.49%  net/http.(*conn).serve
#          0     0%     0%    12.52MB 92.49%  net/http.serverHandler.ServeHTTP
#          0     0%     0%       12MB 88.68%  bytes.(*Buffer).ReadFrom
#          0     0%     0%       12MB 88.68%  bytes.(*Buffer).grow
#       12MB 88.68% 88.68%       12MB 88.68%  bytes.makeSlice
#          0     0% 88.68%       12MB 88.68%  github.com/TykTechnologies/tyk/gateway.copyBody
#          0     0% 88.68%       12MB 88.68%  github.com/TykTechnologies/tyk/gateway.copyRequest
# (pprof) 



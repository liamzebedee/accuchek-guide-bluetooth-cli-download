set -ex

echo Get meter and press button to turn on
./.venv/bin/python3 download.py

timeout 600 go run report/web.go &
# Capture its PID immediately
WEB_PID=$!
echo $WEB_PID
# kill $WEB_PID

open http://localhost:10943/

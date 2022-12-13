#!/bin/bash
#
# A debugging script to run the autograder worker daemon locally without
# docker and without a message queue.
# It assumes that the assets (autograder scripts) has been already
# generated in the directory ./tmp/autograder (use can use ./rebuild.sh).
# It also assumes that virtualenv installation is at ../venv.
#
# Usage:
#
#   ./build-student.sh
#   ./start-server-no-queue.sh

cd "$(dirname "$0")"
DIR="$(pwd -P)"
source ../venv/bin/activate

function @execute() { echo "$@" >&2; "$@"; }

set -e

# Start Jupyter notebook server
pgrep jupyter &>/dev/null || jupyter notebook &

cd go
mkdir -p "$DIR/tmp/uploads" "$DIR/tmp/scratch"

# Stop the processes we started on Ctrl+C
trap 'kill %1' SIGINT

if [ ! -f "$DIR/deploy/local.env" ]; then
  echo "Please copy deploy/secret.env.template " >&2
  echo "to deploy/local.env and customize it." >&2
  exit 1
fi

. "$DIR/deploy/local.env"
export COOKIE_AUTH_KEY COOKIE_ENCRYPT_KEY CLIENT_ID CLIENT_SECRET JWT_KEY

# Create directories.
mkdir -p "$DIR/tmp/scratch" "$DIR/tmp/uploads"

# Start the upload server
test -f "$DIR/deploy/localhost.crt" && test -f "$DIR/deploy/localhost.key" || {
	echo "Could not find localshot cert and key in $DIR/deploy" >&2
  echo "Please create one with this command:" >&2
	echo "  openssl req -x509 -newkey rsa:4096 -keyout $DIR/deploy/localhost.key -out $DIR/deploy/localhost.crt -days 365 -subj '/CN=localhost' -nodes" >&2
	exit 1
}
@execute go run cmd/uploadserver/main.go \
  --logtostderr --v=5 \
  --upload_dir="$DIR/tmp/uploads" \
  --allow_cors \
  --use_openid \
  --static_dir="$DIR/static" \
  --grade_locally \
  --autograder_dir="$DIR/tmp/autograder" \
  --scratch_dir="$DIR/tmp/scratch" \
  --python_path="$(which python3)" \
  --nsjail_path="$(which nsjail)" \
  --disable_cleanup \
  --auto_remove \
  --use_jwt \
  --ssl_cert_file $DIR/deploy/localhost.crt \
  --ssl_key_file $DIR/deploy/localhost.key \
  --use_https

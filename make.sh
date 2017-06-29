#! /bin/sh

# Go 1.8+ required. Ubuntu installation:
#  sudo add-apt-repository ppa:longsleep/golang-backports
#  sudo apt-get update
#  sudo apt-get install golang-go

[ $# -eq 0 -o $# -eq 1 -a "$1" = run ] || (echo "Usage: $0 [run]" >&2; false) || exit 1

REPOPATH="$(dirname "$0")"
REPOPATH="$(cd "$REPOPATH" && pwd)"

(
set -x

# Format code
gofmt -w "$REPOPATH" &&

# Create workspace
export GOPATH="$REPOPATH/build" &&
export GO15VENDOREXPERIMENT=1 &&
mkdir -p "$GOPATH/src/github.com/mgoltzsche/runc-compose" &&
ln -sf "$REPOPATH"/* "$GOPATH/src/github.com/mgoltzsche/runc-compose/" &&
ln -sf "$REPOPATH/vendor.conf" "$GOPATH/vendor.conf" &&
rm -f "$REPOPATH/build/src/github.com/mgoltzsche/runc-compose/build" || exit 1

# Fetch dependencies
if [ ! -f ./build/vndr ]; then
	# Fetch and build dependency manager
	VNDR="$GOPATH/vndr"
	go get github.com/LK4D4/vndr &&
	go build -o "$VNDR" github.com/LK4D4/vndr &&
	# Fetch dependencies
	(
		cd "$GOPATH/src/github.com/mgoltzsche/runc-compose" &&
		"$VNDR" \
			-whitelist='github.com/go-yaml/yaml/.*' \
			-whitelist='github.com/projectatomic/skopeo/.*'
	) || exit 1
fi

# Build linked binary to $GOPATH/bin/rkt-compose
go build -o bin/runc-compose github.com/mgoltzsche/runc-compose &&

# Build and run tests
go test github.com/mgoltzsche/runc-compose/checks &&
go test github.com/mgoltzsche/runc-compose/model &&
go test github.com/mgoltzsche/runc-compose/launcher
) || exit 1

# Run
if [ "$1" = run ]; then
	set -x
	sudo "$REPOPATH/bin/runc-compose" -verbose=true -name=examplepod -uuid-file=/var/run/examplepod.uuid run test-resources/example-docker-compose-images.yml
else
	cat <<-EOF
		___________________________________________________

		runc-compose has been built and tested successfully!
		runc-compose must be run as root.

		Expose binary in \$PATH:
		  export PATH="$REPOPATH/bin:\$PATH"

		Run example pod:
		  runc-compose -name=examplepod -uuid-file=/var/run/examplepod.uuid run test-resources/example-docker-compose-images.yml

		Run consul and example pod registered at consul (requires free IP: 172.16.28.2):
		  runc-compose -name=consul -uuid-file=/var/run/consul.uuid -net=default:IP=172.16.28.2 run test-resources/consul.yml &
		  runc-compose -name=examplepod -uuid-file=/var/run/example.uuid -consul-ip=172.16.28.2 run test-resources/example-docker-compose-images.yml
	EOF
fi

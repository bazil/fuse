#!/bin/bash
#

# Communicating between travis and UML containers.
CURDIR="$(pwd)"
STATUS_FILE="$CURDIR/umltest.status"
INNER_SCRIPT="$CURDIR/umltest.inner.sh"

cat > "$INNER_SCRIPT" <<EOF
#!/bin/bash
(
   set -e
   set -x
   set -o pipefail

   # Enable fuse. Note the call to uname -r does not take place
   # until running inside the UML container.
   insmod /usr/lib/uml/modules/\`uname -r\`/kernel/fs/fuse/fuse.ko

   # Navigate to the current directory
   cd "$CURDIR"

   # Mount the processes of the external environment
   mount -t proc proc /proc

   # Enable the network
   ifconfig lo up; ifconfig eth0 10.0.2.15; ip route add default via 10.0.2.1

   # Embed some environment variables into the UML script runner so
   # they are visible to the test suite. Definitely needed: PATH GOPATH TRAVIS.
   # The other TRAVIS ones are included to minimize gratuitous differences
   # between test environments at the various container inception levels.
   $(declare -xp | egrep '[-]x (GO|TRAVIS|PATH)')

   # Run tests
   go test -v . ./fs |egrep 'SKIP|PASS|FAIL'
)
echo "\$?" > "$STATUS_FILE"
halt -f
EOF

chmod +x "$INNER_SCRIPT"

# Execute the script in user mode linux
/usr/bin/linux.uml init="$INNER_SCRIPT" eth0=slirp rootfstype=hostfs mem=384m rw 2>&1 | \
   egrep -v 'modprobe: FATAL: Could not load /lib/modules|\[no test files\]'

exit $(cat $STATUS_FILE)

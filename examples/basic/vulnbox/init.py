#!/usr/bin/env python3

import subprocess
import json
import os
import time


def main(args):
    teamIp, teamName = args

    with open("data.json", "w") as f:
        json.dump({"team": {"name": teamName, "ip": teamIp}}, f)

    subprocess.check_call(
        ["openrc"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL
    )
    subprocess.check_call(["touch", "/run/openrc/softlevel"])

    services = [
        ("pastebin", 5000, "/usr/bin/python3", "/root/pastebin/app.py", True),
        # ("post_office", 5001, "/usr/bin/python3", "/root/post_office/app.py", True),
        ("pcap-broker", 4242, "/root/pcap-broker/pcap-broker", "-cmd \\\"tcpdump -i eth0 -n --immediate-mode -s 65535 -U -w - '! (dst 10.42.0.2 and (dst port 2222 or dst port 4242)) and ! (src 10.42.0.2 and (src port 2222 or src port 4242))'\\\" -listen 0.0.0.0:4242", False),
    ]

    for service, port, cmd, args, check in services:
        with open(f"/etc/init.d/{service}", "w") as f:
            f.write(
                f"""#!/sbin/openrc-run
command="{cmd}"
command_args="{args}"
command_background="yes"
pidfile="/run/{service}.pid"
respawn="yes"
respawn_delay="5"
# Set the working directory to the directory of the script
directory="/root/{service}"
"""
            )

        os.chmod(f"/etc/init.d/{service}", 0o755)

        subprocess.check_call(
            ["service", service, "start"],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )

        # Keep requesting the service until it has either started or crashed
        STARTED = "* status: started"
        status = STARTED
        while True:
            # Check if the service is running
            response = subprocess.run(
                ["service", service, "status"],
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE
            )
            if response.returncode != 0:
                print(f"failed: {service}:", response.stderr)
                return
            status = response.stdout.decode().strip()

            if status != STARTED:
                print(f"failed: {status}")
                return

            if not check:
                break

            # Request the homepage of the service
            failed = False
            try:
                response = subprocess.run(
                    ["curl", f"http://localhost:{port}/"],
                    stdout=subprocess.PIPE,
                    stderr=subprocess.PIPE,
                    timeout=0.5
                )
                if response.returncode != 0:
                    failed = True
            except Exception:
                failed = True

            if not failed:
                break
            else:
                time.sleep(0.5)

    print("success")


if __name__ == "__main__":
    import sys

    try:
        main(sys.argv[1:])
    except Exception as e:
        print(f"exception: {e}")

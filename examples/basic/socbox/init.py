#!/usr/bin/env python3

import subprocess
import os
import time
import json


def main(args):
    teamIp = args[0]

    subprocess.check_call(
        ["openrc"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL
    )
    subprocess.check_call(["touch", "/run/openrc/softlevel"])

    service = "pkappa2"
    port = 80
    check = True

    os.mkdir("/root/pkappa2/data")
    with open("/root/pkappa2/data/2025-05-06_235911.401.2.state.json", "w") as f:
        config = json.dumps({
            "Saved": "2025-05-06T23:59:11.401774+10:00",
            "Tags": [
                {
                    "Name": "tag/flag_out",
                    "Definition": "sdata:\"(flag{.*})|(flag%7B.*%7D)\"",
                    "Matches": None,
                    "Color": "#ff6666",
                    "Converters": []
                },
                {
                    "Name": "tag/flag_in",
                    "Definition": "cdata:\"(flag{.*})|(flag%7B.*%7D)\"",
                    "Matches": None,
                    "Color": "#66ff66",
                    "Converters": []
                },
                {
                    "Name": "service/pastebin",
                    "Definition": "sport:5000",
                    "Matches": None,
                    "Color": "#6672ff",
                    "Converters": []
                },
                # {
                #     "Name": "service/post office",
                #     "Definition": "sport:5001",
                #     "Matches": None,
                #     "Color": "#66ff9a",
                #     "Converters": []
                # }
            ],
            "PcapProcessorWebhookUrls": None,
            "PcapOverIPEndpoints": [f"{teamIp}:4242"],
            "Config": {
                "AutoInsertLimitToQuery": False
            }
        })
        f.write(config)

    with open(f"/etc/init.d/{service}", "w") as f:
        f.write(
            f"""#!/sbin/openrc-run
command="/root/{service}/main"
command_args="-base_dir data -address 0.0.0.0:80"
command_background="yes"
pidfile="/run/{service}.pid"
respawn="yes"
respawn_delay="5"
# Set the working directory to the directory of pkappa2
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

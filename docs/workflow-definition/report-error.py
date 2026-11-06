"""Report an engine-redacted workflow error received as JSON on stdin."""

import json
import sys


def main():
    error = json.load(sys.stdin)["error"]
    json.dump({"code": error["code"], "message": error["message"]}, sys.stderr)
    sys.stderr.write("\n")


if __name__ == "__main__":
    main()

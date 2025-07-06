#!/bin/bash

# Check if prompt is provided
if [ $# -eq 0 ]; then
    echo "Usage: $0 \"<prompt>\""
    echo "Example: $0 \"explain this code\""
    exit 1
fi

# Get the prompt from command line argument
PROMPT="$1"

# Get current working directory
CWD=$(pwd)

# Build the payload JSON
PAYLOAD=$(printf '{"prompt":"%s","cwd":"%s"}' "$PROMPT" "$CWD")

# Execute the enqueue command
go run ./main.go enqueue --database-url=postgresql:///good_jobs --payload="$PAYLOAD"
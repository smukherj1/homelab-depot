## Agent

Agent that runs on a remote machine that allows executing commands on
the remote machine in a sandboxed environment. Agent provides a GRPC
service that allows:

- Creating a "session" which is the sandboxed environment. A sandbox
  has an id which must be provided in every session related method.
  Right now, only one session may be active at a time.
- Uploading files to the sandboxed environment.
- Downloading files from the sandboxed environment.
- Running a command (using an uploaded executable file) in the
  sandboxed environment.
- Deleting the session which deletes all uploaded files.

The Agent sandbox works as follows:

- The agent creates a temporary directory on startup.
- A directory for a session is created within this temporary directory when
  a session is created.
- All uploaded files are created in this session specific directory. Files
  can only be downloaded from this session specific directory.
- The agent uses docker to sandbox the command running where it mounts the
  session directory in the container, and runs the command within the container.
  Commands are executed in the ubuntu:26.04 image.
  with the session directory as the working directory.

## Tech Stack

- Golang for server code.
- ? for build system.

## Modules

### GRPC Server

Main server. Has a single mutex protected session that's created when
CreateSession is called and set to nil when the session is completed
or expires.

Also uses the mutex to protect the session expiry time which is updated
whenever a session scoped method is called or in progress. For streaming methods, a
Go routine is launched that extends the session expiry every minute by
5 mins. The Go routine is cleaned up before the RPC returns / completes. This ensures
running a session method that takes longer than 5 mins doesn't expire the session.

### Session Manager

Responsible for managing a session. Created by the GRPC server
when a session is created. Creates a dedicated temporary directory which
it deletes in a Cleanup method (called by the GRPC server on session completion).

Handles uploading, downloading files and running commands in a sandbox for that
session.

### Sandbox.

Abstracts away the sandboxing technology being used is Docker. Allows configuring
and launching a sandbox. Configuration includes:

- Directories or files to mount and their path mappings (host <-> sandbox path).
- The command to run.

Use docker as the sandboxing technology with a ubuntu:26.04 image.

## Directory / Module Structure

(Paths relative to "remote-agent" directory.)

- bin/agent (Binary entrypoint / main function for the agent)
- internal/
  - service
    - agent: GRPC server for the agent.proto service.
  - session- Agent session manager.
  - sandbox- Sandbox manager.
- generated (Generated code. e.g., proto and GRPC generated Go code.)

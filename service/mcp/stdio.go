package mcp

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/google/shlex"
	"github.com/google/uuid"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"gorm.io/gorm"
)

var imageVersion = regexp.MustCompile(`^([a-z0-9.-]+(:[0-9]+)?(/[a-z0-9]+([._-][a-z0-9]+)*)+):([0-9][A-Za-z0-9._-]{0,63})$`)
var imageDigest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
var stdioSlots = make(chan struct{}, 16)

func StdioCommand(connection model.MCPConnection) ([]string, error) {
	if len(connection.Package) > 256 || !imageVersion.MatchString(connection.Package) || len(connection.Command) > 4096 || strings.ContainsAny(connection.Command, "\x00\r\n") {
		return nil, Failure(422, "INVALID_PACKAGE", "Supply a versioned registry image and command")
	}
	args, err := shlex.Split(connection.Command)
	if err != nil || len(args) == 0 || len(args) > 128 || args[0] == "" || strings.HasPrefix(args[0], "-") {
		return nil, Failure(422, "INVALID_COMMAND", "Invalid command or quoted argument")
	}
	return args, nil
}

func trustedImage(reference string) bool {
	for item := range strings.SplitSeq(os.Getenv("MCP_STDIO_PACKAGE_SOURCES"), ",") {
		prefix := strings.TrimRight(strings.TrimSpace(item), "/")
		if prefix != "" && (reference == prefix || strings.HasPrefix(reference, prefix+"/")) {
			return true
		}
	}
	return false
}

// The registry is contacted only for explicitly allowed repositories. The
// stored manifest digest is also used on other nodes and after cache eviction.
func InstallPackage(ctx context.Context, id string, revision int64) (*model.MCPServer, error) {
	server, err := model.GetMCPServer(id)
	if err != nil {
		return nil, err
	}
	if server.Revision != revision {
		return nil, model.ErrMCPRevisionConflict
	}
	conn := server.Connection
	image := imageVersion.FindStringSubmatch(conn.Package)
	if conn.Transport != "stdio" || len(image) == 0 || !trustedImage(image[1]) {
		return nil, Failure(422, "UNTRUSTED_PACKAGE", "Package repository is not enabled on this deployment")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(server.Limits.TimeoutSeconds)*time.Second)
	defer cancel()
	reference := conn.Package
	if conn.PackageDigest != "" {
		reference = image[1] + "@" + conn.PackageDigest
	}
	if err = exec.CommandContext(ctx, "docker", "pull", "--quiet", reference).Run(); err != nil {
		return nil, Failure(502, "PACKAGE_DOWNLOAD_FAILED", "The configured package version could not be downloaded")
	}
	digests, err := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{json .RepoDigests}}", reference).Output()
	var entries []string
	if err != nil || common.Unmarshal(digests, &entries) != nil {
		return nil, Failure(502, "INVALID_PACKAGE", "Could not verify the downloaded package")
	}
	for _, entry := range entries {
		repo, digest, ok := strings.Cut(entry, "@")
		if ok && repo == image[1] && imageDigest.MatchString(digest) {
			if conn.PackageDigest == digest {
				return server, nil
			}
			conn.PackageDigest = digest
			return updateCatalogConnection(id, revision, map[string]any{"connection": conn, "connection_revision": gorm.Expr("connection_revision + 1"), "connection_health": nil, "enabled": false})
		}
	}
	return nil, Failure(502, "INVALID_PACKAGE", "Downloaded image does not match the configured repository")
}

type stdioOutput struct {
	io.Reader
	io.Closer
}

func connectStdio(ctx context.Context, server *model.MCPServer) (*sdk.ClientSession, func(), error) {
	conn := server.Connection
	args, err := StdioCommand(conn)
	if err != nil {
		return nil, nil, err
	}
	image := imageVersion.FindStringSubmatch(conn.Package)
	if !trustedImage(image[1]) || !imageDigest.MatchString(conn.PackageDigest) {
		return nil, nil, Failure(422, "PACKAGE_REQUIRED", "Install the configured package before connecting")
	}
	secrets, err := serverCredentials(server)
	if err != nil {
		return nil, nil, err
	}
	select {
	case stdioSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	name := "new-api-mcp-" + uuid.NewString()
	cleanup := func() {
		stop, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = exec.CommandContext(stop, "docker", "rm", "-f", name).Run()
		<-stdioSlots
	}
	options := []string{"create", "--pull=missing", "-i", "--name", name, "--network=none", "--read-only", "--user=65534:65534", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--pids-limit=64", "--memory=128m", "--memory-swap=128m", "--cpus=1", "--log-driver=none", "--tmpfs=/tmp:rw,noexec,nosuid,nodev,size=16m", "--workdir=/tmp"}
	if len(secrets.Env) > 0 {
		file, err := os.CreateTemp("", "mcp-env-*")
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		defer os.Remove(file.Name())
		for key, value := range secrets.Env {
			if !variableName.MatchString(key) || strings.ContainsAny(value, "\r\n\x00") {
				err = fmt.Errorf("invalid environment entry")
				break
			}
			if _, err = fmt.Fprintf(file, "%s=%s\n", key, value); err != nil {
				break
			}
		}
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			cleanup()
			return nil, nil, Failure(422, "INVALID_CREDENTIAL", "Unable to configure the service environment")
		}
		options = append(options, "--env-file", file.Name())
	}
	options = append(options, "--entrypoint", args[0], image[1]+"@"+conn.PackageDigest)
	options = append(options, args[1:]...)
	if err := exec.CommandContext(ctx, "docker", options...).Run(); err != nil {
		cleanup()
		return nil, nil, Failure(502, "STDIO_START_FAILED", "The isolated MCP container could not be created")
	}
	cmd := exec.CommandContext(ctx, "docker", "start", "-a", "-i", name)
	out, err := cmd.StdoutPipe()
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		out.Close()
		cleanup()
		return nil, nil, err
	}
	if err = cmd.Start(); err != nil {
		in.Close()
		out.Close()
		cleanup()
		return nil, nil, Failure(502, "STDIO_START_FAILED", "The isolated MCP container could not be started")
	}
	transport := &sdk.IOTransport{Reader: &stdioOutput{Reader: io.LimitReader(out, MaxResultBytes+1), Closer: out}, Writer: in}
	session, err := sdk.NewClient(&sdk.Implementation{Name: "new-api-mcp", Version: common.Version}, nil).Connect(ctx, transport, nil)
	closeProcess := func() { in.Close(); out.Close(); cleanup(); _ = cmd.Wait() }
	if err != nil {
		closeProcess()
		return nil, nil, Failure(502, "UPSTREAM_CONNECT_FAILED", "Isolated MCP handshake failed")
	}
	return session, func() { _ = session.Close(); closeProcess() }, nil
}

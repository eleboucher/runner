package testplugin

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	pluginv1 "code.forgejo.org/forgejo/runner/v12/act/plugin/proto/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type environment struct {
	root    string // temp directory root
	workdir string
	env     []string
}

type Server struct {
	pluginv1.UnimplementedBackendPluginServer

	mu   sync.Mutex
	envs map[string]*environment
	seq  int
}

func New() *Server {
	return &Server{
		envs: make(map[string]*environment),
	}
}

func (s *Server) getEnv(id string) (*environment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	env, ok := s.envs[id]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "environment %q not found", id)
	}
	return env, nil
}

// resolvePath maps container paths (absolute or relative) into the temp root.
func resolvePath(env *environment, p string) string {
	return filepath.Join(env.root, p)
}

func (s *Server) Capabilities(_ context.Context, _ *pluginv1.CapabilitiesRequest) (*pluginv1.CapabilitiesResponse, error) {
	return &pluginv1.CapabilitiesResponse{
		Name:                       "test",
		RootPath:                   "/shared",
		ActPath:                    "/shared/act",
		ToolCachePath:              "/shared/toolcache",
		PathVariableName:           "PATH",
		DefaultPathVariable:        "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		PathSeparator:              ":",
		SupportsDockerActions:      false,
		ManagesOwnNetworking:       true,
		SupportsServiceContainers:  false,
		EnvironmentCaseInsensitive: runtime.GOOS == "windows",
		SupportsLocalCopy:          true,
		RunnerContext: map[string]string{
			"os":         "Linux",
			"arch":       "x86_64",
			"temp":       "/tmp",
			"tool_cache": "/shared/toolcache",
		},
	}, nil
}

func (s *Server) Create(_ context.Context, req *pluginv1.CreateRequest) (*pluginv1.CreateResponse, error) {
	root, err := os.MkdirTemp("", "testplugin-*")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create tmpdir: %v", err)
	}

	workdir := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		os.RemoveAll(root)
		return nil, status.Errorf(codes.Internal, "create workdir: %v", err)
	}

	for _, dir := range []string{"shared/act", "shared/toolcache", "shared/workdir"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			os.RemoveAll(root)
			return nil, status.Errorf(codes.Internal, "create %s: %v", dir, err)
		}
	}

	s.mu.Lock()
	s.seq++
	envID := fmt.Sprintf("test-env-%d", s.seq)
	s.envs[envID] = &environment{
		root:    root,
		workdir: workdir,
		env:     req.GetEnv(),
	}
	s.mu.Unlock()

	return &pluginv1.CreateResponse{EnvironmentId: envID}, nil
}

func (s *Server) Start(_ context.Context, _ *pluginv1.StartRequest) (*pluginv1.StartResponse, error) {
	return &pluginv1.StartResponse{}, nil
}

func (s *Server) Exec(req *pluginv1.ExecRequest, stream grpc.ServerStreamingServer[pluginv1.ExecOutput]) error {
	env, err := s.getEnv(req.GetEnvironmentId())
	if err != nil {
		return err
	}

	command := req.GetCommand()
	if len(command) == 0 {
		return sendExecDone(stream, 1, "empty command")
	}

	wd := req.GetWorkdir()
	if wd == "" {
		wd = env.workdir
	} else {
		wd = resolvePath(env, wd)
	}
	os.MkdirAll(wd, 0o755)

	envList := os.Environ()
	envList = append(envList, env.env...)
	for k, v := range req.GetEnv() {
		envList = append(envList, k+"="+v)
	}

	cmd := exec.CommandContext(stream.Context(), command[0], command[1:]...)
	cmd.Dir = wd
	cmd.Env = envList

	var stdoutMu sync.Mutex
	cmd.Stdout = &execStreamWriter{mu: &stdoutMu, stream: stream, streamType: pluginv1.ExecOutput_STDOUT}
	cmd.Stderr = &execStreamWriter{mu: &stdoutMu, stream: stream, streamType: pluginv1.ExecOutput_STDERR}

	runErr := cmd.Run()

	exitCode := int32(0)
	errorMsg := ""
	if runErr != nil {
		exitCode = 1
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			exitCode = int32(exitErr.ExitCode())
		} else {
			errorMsg = runErr.Error()
		}
	}

	return sendExecDone(stream, exitCode, errorMsg)
}

func sendExecDone(stream grpc.ServerStreamingServer[pluginv1.ExecOutput], exitCode int32, errorMsg string) error {
	return stream.Send(&pluginv1.ExecOutput{
		Done:         true,
		ExitCode:     exitCode,
		ErrorMessage: errorMsg,
	})
}

func (s *Server) CopyIn(stream grpc.ClientStreamingServer[pluginv1.CopyInChunk, pluginv1.CopyInResponse]) error {
	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "copyin recv: %v", err)
	}

	env, err := s.getEnv(first.GetEnvironmentId())
	if err != nil {
		return err
	}

	destPath := resolvePath(env, first.GetDestPath())

	var buf bytes.Buffer
	if len(first.GetData()) > 0 {
		buf.Write(first.GetData())
	}
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return status.Errorf(codes.Internal, "copyin recv: %v", err)
		}
		buf.Write(chunk.GetData())
	}

	if err := extractTar(destPath, &buf); err != nil {
		return status.Errorf(codes.Internal, "copyin extract: %v", err)
	}

	return stream.SendAndClose(&pluginv1.CopyInResponse{})
}

func (s *Server) CopyLocal(_ context.Context, req *pluginv1.CopyLocalRequest) (*pluginv1.CopyLocalResponse, error) {
	env, err := s.getEnv(req.GetEnvironmentId())
	if err != nil {
		return nil, err
	}

	destPath := resolvePath(env, req.GetDestPath())

	if err := copyDir(destPath, req.GetSrcPath()); err != nil {
		return nil, status.Errorf(codes.Internal, "copylocal: %v", err)
	}

	return &pluginv1.CopyLocalResponse{}, nil
}

func (s *Server) CopyOut(req *pluginv1.CopyOutRequest, stream grpc.ServerStreamingServer[pluginv1.CopyOutChunk]) error {
	env, err := s.getEnv(req.GetEnvironmentId())
	if err != nil {
		return err
	}

	srcPath := resolvePath(env, req.GetSrcPath())

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	fi, err := os.Stat(srcPath)
	if err != nil {
		return status.Errorf(codes.NotFound, "copyout stat: %v", err)
	}

	if fi.IsDir() {
		err = filepath.WalkDir(srcPath, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(srcPath, path)
			info, err := d.Info()
			if err != nil {
				return err
			}
			hdr := &tar.Header{
				Name: rel,
				Mode: int64(info.Mode()),
				Size: info.Size(),
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(tw, f)
			return err
		})
	} else {
		hdr := &tar.Header{
			Name: fi.Name(),
			Mode: int64(fi.Mode()),
			Size: fi.Size(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return status.Errorf(codes.Internal, "copyout header: %v", err)
		}
		f, err := os.Open(srcPath)
		if err != nil {
			return status.Errorf(codes.Internal, "copyout open: %v", err)
		}
		defer f.Close()
		if _, err := io.Copy(tw, f); err != nil {
			return status.Errorf(codes.Internal, "copyout copy: %v", err)
		}
	}
	if err != nil {
		return status.Errorf(codes.Internal, "copyout walk: %v", err)
	}
	tw.Close()

	data := buf.Bytes()
	const chunkSize = 256 * 1024
	for i := 0; i < len(data); i += chunkSize {
		end := i + chunkSize
		if end > len(data) {
			end = len(data)
		}
		if err := stream.Send(&pluginv1.CopyOutChunk{Data: data[i:end]}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) UpdateEnv(_ context.Context, req *pluginv1.UpdateEnvRequest) (*pluginv1.UpdateEnvResponse, error) {
	env, err := s.getEnv(req.GetEnvironmentId())
	if err != nil {
		return nil, err
	}

	srcPath := resolvePath(env, req.GetSrcPath())

	current := req.GetCurrentEnv()
	if current == nil {
		current = make(map[string]string)
	}

	data, err := os.ReadFile(srcPath)
	if err != nil {
		return &pluginv1.UpdateEnvResponse{UpdatedEnv: current}, nil
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if idx := strings.Index(line, "="); idx > 0 {
			current[line[:idx]] = line[idx+1:]
		}
	}

	return &pluginv1.UpdateEnvResponse{UpdatedEnv: current}, nil
}

func (s *Server) IsHealthy(_ context.Context, req *pluginv1.IsHealthyRequest) (*pluginv1.IsHealthyResponse, error) {
	if _, err := s.getEnv(req.GetEnvironmentId()); err != nil {
		return nil, err
	}
	return &pluginv1.IsHealthyResponse{WaitNanos: 0}, nil
}

func (s *Server) Remove(_ context.Context, req *pluginv1.RemoveRequest) (*pluginv1.RemoveResponse, error) {
	envID := req.GetEnvironmentId()

	s.mu.Lock()
	env, ok := s.envs[envID]
	if ok {
		delete(s.envs, envID)
	}
	s.mu.Unlock()

	if !ok {
		return nil, status.Errorf(codes.NotFound, "environment %q not found", envID)
	}

	os.RemoveAll(env.root)
	return &pluginv1.RemoveResponse{}, nil
}

func (s *Server) Cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, env := range s.envs {
		os.RemoveAll(env.root)
	}
	clear(s.envs)
}

type execStreamWriter struct {
	mu         *sync.Mutex
	stream     grpc.ServerStreamingServer[pluginv1.ExecOutput]
	streamType pluginv1.ExecOutput_Stream
}

func (w *execStreamWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.stream.Send(&pluginv1.ExecOutput{
		Stream: w.streamType,
		Data:   p,
	}); err != nil {
		return 0, err
	}
	return len(p), nil
}

func extractTar(destPath string, r io.Reader) error {
	if err := os.MkdirAll(destPath, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(destPath, filepath.Clean(hdr.Name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fs.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		}
	}
}

func copyDir(dst, src string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

var _ pluginv1.BackendPluginServer = (*Server)(nil)

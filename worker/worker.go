package worker

import (
	"context"
	"fmt"
	"net"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/airbloc/logger"
	"github.com/airbloc/logger/module/loggergrpc"
	"github.com/golang/protobuf/ptypes/empty"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	"github.com/pkg/errors"
	"github.com/therne/lrmr/coordinator"
	"github.com/therne/lrmr/input"
	"github.com/therne/lrmr/internal/serialization"
	"github.com/therne/lrmr/job"
	"github.com/therne/lrmr/lrmrpb"
	"github.com/therne/lrmr/node"
	"github.com/therne/lrmr/output"
	"github.com/therne/lrmr/stage"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var log = logger.New("lrmr")

type Worker struct {
	nodeManager node.Manager
	jobManager  *job.Manager
	jobReporter *job.Reporter
	server      *grpc.Server

	runningTasksMu  sync.RWMutex
	runningTasks    map[string]*TaskExecutor
	workerLocalOpts map[string]interface{}

	opt Options
}

func New(crd coordinator.Coordinator, opt Options) (*Worker, error) {
	nm, err := node.NewManager(crd, node.DefaultManagerOptions())
	if err != nil {
		return nil, err
	}
	srv := grpc.NewServer(
		grpc.MaxRecvMsgSize(opt.Input.MaxRecvSize),
		grpc.UnaryInterceptor(loggergrpc.UnaryServerRecover()),
		grpc.StreamInterceptor(grpc_middleware.ChainStreamServer(
			errorLogMiddleware,
			loggergrpc.StreamServerRecover(),
		)),
	)
	return &Worker{
		nodeManager:     nm,
		jobReporter:     job.NewJobReporter(crd),
		jobManager:      job.NewManager(nm, crd),
		server:          srv,
		runningTasks:    make(map[string]*TaskExecutor),
		workerLocalOpts: make(map[string]interface{}),
		opt:             opt,
	}, nil
}

func (w *Worker) SetWorkerLocalOption(key string, value interface{}) {
	w.workerLocalOpts[key] = value
}

func (w *Worker) Start() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	lrmrpb.RegisterNodeServer(w.server, w)
	lis, err := net.Listen("tcp", w.opt.ListenHost)
	if err != nil {
		return err
	}
	advHost := w.opt.AdvertisedHost
	if strings.HasSuffix(advHost, ":") {
		// port is assigned automatically
		addrFrags := strings.Split(lis.Addr().String(), ":")
		advHost += addrFrags[len(addrFrags)-1]
	}

	n := node.New(advHost, node.Worker)
	n.Tag = w.opt.NodeTags
	if err := w.nodeManager.RegisterSelf(ctx, n); err != nil {
		return fmt.Errorf("register worker: %w", err)
	}
	w.jobReporter.Start()
	return w.server.Serve(lis)
}

func (w *Worker) CreateTasks(ctx context.Context, req *lrmrpb.CreateTasksRequest) (*empty.Empty, error) {
	var s stage.Stage
	if err := req.Stage.UnmarshalJSON(&s); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid stage JSON: %v", err)
	}
	broadcasts, err := serialization.DeserializeBroadcast(req.Broadcasts)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	wg, wctx := errgroup.WithContext(ctx)
	for _, p := range req.PartitionIDs {
		partitionID := p
		wg.Go(func() error { return w.createTask(wctx, req, partitionID, s, broadcasts) })
	}
	if err := wg.Wait(); err != nil {
		return nil, err
	}
	log.Info("Create {}/{}/[{}] (Job ID: {})", req.Job.Name, s.Name, strings.Join(req.PartitionIDs, ","), req.Job.Id)
	return &empty.Empty{}, nil
}

func (w *Worker) createTask(ctx context.Context, req *lrmrpb.CreateTasksRequest, partitionID string, s stage.Stage, broadcasts serialization.Broadcast) error {
	task := job.NewTask(partitionID, w.nodeManager.Self(), req.Job.Id, s)
	ts, err := w.jobManager.CreateTask(ctx, task)
	if err != nil {
		return status.Errorf(codes.Internal, "create task failed: %v", err)
	}
	w.jobReporter.Add(task.Reference(), ts)

	c := newTaskContext(context.Background(), w, task, broadcasts)
	in := input.NewReader(w.opt.Input.QueueLength)
	out, err := w.newOutputWriter(c, req.Job.Id, s, req.Output)
	if err != nil {
		return status.Errorf(codes.Internal, "unable to create output: %v", err)
	}

	exec, err := NewTaskExecutor(c, task, s.Function, in, out)
	if err != nil {
		err = errors.Wrap(err, "failed to start executor")
		if reportErr := w.jobReporter.ReportFailure(task.Reference(), err); reportErr != nil {
			return reportErr
		}
		return err
	}
	w.runningTasksMu.Lock()
	w.runningTasks[task.Reference().String()] = exec
	w.runningTasksMu.Unlock()

	go exec.Run()
	return nil
}

func (w *Worker) newOutputWriter(ctx *taskContext, jobID string, cur stage.Stage, o *lrmrpb.Output) (output.Output, error) {
	idToOutput := make(map[string]output.Output)
	for id, host := range o.PartitionToHost {
		taskID := path.Join(jobID, cur.Output.Stage, id)
		if host == w.nodeManager.Self().Host {
			t := w.getRunningTask(taskID)
			if t != nil {
				idToOutput[id] = NewLocalPipe(t.Input)
				continue
			}
		}
		out, err := output.NewPushStream(ctx, w.nodeManager, host, taskID)
		if err != nil {
			return nil, err
		}
		idToOutput[id] = output.NewBufferedOutput(out, w.opt.Output.BufferLength)
	}
	return output.NewWriter(ctx, partitions.UnwrapPartitioner(cur.Output.Partitioner), idToOutput), nil
}

func (w *Worker) getRunningTask(taskID string) *TaskExecutor {
	w.runningTasksMu.RLock()
	defer w.runningTasksMu.RUnlock()
	return w.runningTasks[taskID]
}

func (w *Worker) PushData(stream lrmrpb.Node_PushDataServer) error {
	h, err := lrmrpb.DataHeaderFromMetadata(stream)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	exec := w.getRunningTask(h.TaskID)
	if exec == nil {
		return status.Errorf(codes.InvalidArgument, "task not found: %s", h.TaskID)
	}
	defer func() {
		w.runningTasksMu.Lock()
		delete(w.runningTasks, h.TaskID)
		w.runningTasksMu.Unlock()
	}()

	in := input.NewPushStream(exec.Input, stream)
	if err := in.Dispatch(exec.context); err != nil {
		return err
	}
	exec.WaitForFinish()
	return stream.SendAndClose(&empty.Empty{})
}

func (w *Worker) PollData(stream lrmrpb.Node_PollDataServer) error {
	h, err := lrmrpb.DataHeaderFromMetadata(stream)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	exec := w.getRunningTask(h.TaskID)
	if exec == nil {
		return status.Errorf(codes.InvalidArgument, "task not found: %s", h.TaskID)
	}
	panic("implement me")
}

func (w *Worker) Stop() error {
	w.server.Stop()
	w.jobReporter.Close()
	if err := w.nodeManager.UnregisterNode(w.nodeManager.Self().ID); err != nil {
		return errors.Wrap(err, "unregister node")
	}
	return w.nodeManager.Close()
}

func errorLogMiddleware(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	// dump header on stream failure
	if err := handler(srv, ss); err != nil {
		if h, err := lrmrpb.DataHeaderFromMetadata(ss); err == nil {
			log.Error(" By {} (From {})", h.TaskID, h.FromHost)
		}
		return err
	}
	return nil
}

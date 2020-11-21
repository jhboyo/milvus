package proxy

import (
	"context"
	"log"
	"math/rand"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"

	"github.com/zilliztech/milvus-distributed/internal/allocator"
	"github.com/zilliztech/milvus-distributed/internal/msgstream"
	"github.com/zilliztech/milvus-distributed/internal/proto/masterpb"
	"github.com/zilliztech/milvus-distributed/internal/proto/servicepb"
	"github.com/zilliztech/milvus-distributed/internal/util/typeutil"
)

type UniqueID = typeutil.UniqueID
type Timestamp = typeutil.Timestamp

type Proxy struct {
	proxyLoopCtx    context.Context
	proxyLoopCancel func()
	proxyLoopWg     sync.WaitGroup

	grpcServer   *grpc.Server
	masterConn   *grpc.ClientConn
	masterClient masterpb.MasterClient
	sched        *TaskScheduler
	tick         *timeTick

	idAllocator  *allocator.IDAllocator
	tsoAllocator *allocator.TimestampAllocator
	segAssigner  *allocator.SegIDAssigner

	manipulationMsgStream *msgstream.PulsarMsgStream
	queryMsgStream        *msgstream.PulsarMsgStream

	// Add callback functions at different stages
	startCallbacks []func()
	closeCallbacks []func()
}

func Init() {
	Params.InitParamTable()
}

func CreateProxy(ctx context.Context) (*Proxy, error) {
	rand.Seed(time.Now().UnixNano())
	ctx1, cancel := context.WithCancel(ctx)
	p := &Proxy{
		proxyLoopCtx:    ctx1,
		proxyLoopCancel: cancel,
	}

	// TODO: use config instead
	pulsarAddress := Params.PulsarAddress()
	bufSize := int64(1000)
	manipulationChannels := []string{"manipulation"}
	queryChannels := []string{"query"}

	p.manipulationMsgStream = msgstream.NewPulsarMsgStream(p.proxyLoopCtx, bufSize)
	p.manipulationMsgStream.SetPulsarClient(pulsarAddress)
	p.manipulationMsgStream.CreatePulsarProducers(manipulationChannels)

	p.queryMsgStream = msgstream.NewPulsarMsgStream(p.proxyLoopCtx, bufSize)
	p.queryMsgStream.SetPulsarClient(pulsarAddress)
	p.queryMsgStream.CreatePulsarProducers(queryChannels)

	masterAddr := Params.MasterAddress()
	idAllocator, err := allocator.NewIDAllocator(p.proxyLoopCtx, masterAddr)

	if err != nil {
		return nil, err
	}
	p.idAllocator = idAllocator

	tsoAllocator, err := allocator.NewTimestampAllocator(p.proxyLoopCtx, masterAddr)
	if err != nil {
		return nil, err
	}
	p.tsoAllocator = tsoAllocator

	segAssigner, err := allocator.NewSegIDAssigner(p.proxyLoopCtx, masterAddr)
	if err != nil {
		panic(err)
	}
	p.segAssigner = segAssigner

	p.sched, err = NewTaskScheduler(p.proxyLoopCtx, p.idAllocator, p.tsoAllocator)
	if err != nil {
		return nil, err
	}

	return p, nil
}

// AddStartCallback adds a callback in the startServer phase.
func (p *Proxy) AddStartCallback(callbacks ...func()) {
	p.startCallbacks = append(p.startCallbacks, callbacks...)
}

func (p *Proxy) startProxy() error {
	err := p.connectMaster()
	if err != nil {
		return err
	}
	initGlobalMetaCache(p.proxyLoopCtx, p.masterClient, p.idAllocator, p.tsoAllocator)
	p.manipulationMsgStream.Start()
	p.queryMsgStream.Start()
	p.sched.Start()
	p.idAllocator.Start()
	p.tsoAllocator.Start()
	p.segAssigner.Start()

	// Start callbacks
	for _, cb := range p.startCallbacks {
		cb()
	}

	p.proxyLoopWg.Add(1)
	go p.grpcLoop()

	return nil
}

// AddCloseCallback adds a callback in the Close phase.
func (p *Proxy) AddCloseCallback(callbacks ...func()) {
	p.closeCallbacks = append(p.closeCallbacks, callbacks...)
}

func (p *Proxy) grpcLoop() {
	defer p.proxyLoopWg.Done()

	// TODO: use address in config instead
	lis, err := net.Listen("tcp", ":5053")
	if err != nil {
		log.Fatalf("Proxy grpc server fatal error=%v", err)
	}

	p.grpcServer = grpc.NewServer()
	servicepb.RegisterMilvusServiceServer(p.grpcServer, p)
	if err = p.grpcServer.Serve(lis); err != nil {
		log.Fatalf("Proxy grpc server fatal error=%v", err)
	}
}

func (p *Proxy) connectMaster() error {
	masterAddr := Params.MasterAddress()
	log.Printf("Proxy connected to master, master_addr=%s", masterAddr)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(ctx, masterAddr, grpc.WithInsecure(), grpc.WithBlock())
	if err != nil {
		log.Printf("Proxy connect to master failed, error= %v", err)
		return err
	}
	log.Printf("Proxy connected to master, master_addr=%s", masterAddr)
	p.masterConn = conn
	p.masterClient = masterpb.NewMasterClient(conn)
	return nil
}

func (p *Proxy) Start() error {
	return p.startProxy()
}

func (p *Proxy) stopProxyLoop() {
	p.proxyLoopCancel()

	if p.grpcServer != nil {
		p.grpcServer.GracefulStop()
	}
	p.tsoAllocator.Close()

	p.idAllocator.Close()

	p.segAssigner.Close()

	p.sched.Close()

	p.manipulationMsgStream.Close()

	p.queryMsgStream.Close()

	p.proxyLoopWg.Wait()
}

// Close closes the server.
func (p *Proxy) Close() {
	p.stopProxyLoop()

	for _, cb := range p.closeCallbacks {
		cb()
	}
	log.Print("proxy closed.")
}

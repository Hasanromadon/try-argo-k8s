package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"time"

	"banking-microservices/pkg/pb"
	"google.golang.org/grpc"
)

// AdapterServiceServer mengimplementasikan pb.VendorAdapterServiceServer
type AdapterServiceServer struct {
	pb.UnimplementedVendorAdapterServiceServer
}

// ExecuteTransfer mensimulasikan tembakan ke sistem Vendor (misal Biller Eksternal)
func (s *AdapterServiceServer) ExecuteTransfer(ctx context.Context, req *pb.TransferRequest) (*pb.TransferResponse, error) {
	log.Printf("[gRPC] Menerima instruksi transfer: %s -> %s sebesar %f\n", req.SourceAccount, req.TargetAccount, req.Amount)

	// Simulasi delay jaringan ke Vendor
	time.Sleep(500 * time.Millisecond)

	// Simulasi respons sukses dari Vendor
	return &pb.TransferResponse{
		TransactionId:     req.TransactionId,
		Status:            pb.TransferStatus_SUCCESS,
		VendorReferenceId: "VND-" + fmt.Sprintf("%d", time.Now().Unix()),
	}, nil
}

// CheckStatus mensimulasikan pengecekan status (Inquiry) ke sistem Vendor
func (s *AdapterServiceServer) CheckStatus(ctx context.Context, req *pb.StatusRequest) (*pb.StatusResponse, error) {
	log.Printf("[gRPC] Mengecek status untuk transaksi: %s\n", req.TransactionId)

	// Simulasi delay
	time.Sleep(200 * time.Millisecond)

	// Simulasi respons status selalu SUCCESS untuk demo
	return &pb.StatusResponse{
		TransactionId:     req.TransactionId,
		Status:            pb.TransferStatus_SUCCESS,
		VendorReferenceId: req.VendorReferenceId,
	}, nil
}

func StartGRPCServer(port int) {
	addr := fmt.Sprintf(":%d", port)
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("Failed to listen on gRPC port: %v", err)
	}

	grpcServer := grpc.NewServer()
	
	// Daftarkan service kita ke server gRPC
	pb.RegisterVendorAdapterServiceServer(grpcServer, &AdapterServiceServer{})

	log.Printf("gRPC Server listening on %s\n", addr)
	
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("Failed to serve gRPC: %v", err)
	}
}

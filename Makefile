# ==============================================================================
# Makefile untuk Banking Microservices
# ==============================================================================

.PHONY: proto clean

# Direktori proto
PROTO_DIR = api/proto
# Direktori hasil generate golang
PB_DIR = pkg/pb

# ==============================================================================
# PROTOC COMPILATION
# ==============================================================================
# Mengkompilasi semua file .proto menjadi file Golang (gRPC)
proto:
	@echo "🛠️  Mengeksekusi Protoc Compiler..."
	@mkdir -p $(PB_DIR)
	protoc --proto_path=$(PROTO_DIR) \
		--go_out=$(PB_DIR) --go_opt=paths=source_relative \
		--go-grpc_out=$(PB_DIR) --go-grpc_opt=paths=source_relative \
		$(PROTO_DIR)/*.proto
	@echo "✅ Generate Protobuf Golang berhasil! Hasil ada di $(PB_DIR)"

clean:
	@echo "🧹 Membersihkan file hasil generate..."
	@rm -rf $(PB_DIR)/*
	@echo "✅ Bersih!"

# ==============================================================================
# DOCKER / PODMAN BUILD
# ==============================================================================
.PHONY: build-images

build-images:
	@echo "🐳 Membangun image untuk Transfer Service..."
	podman build -t bank-transfer-service:latest -f services/transfer-service/Containerfile .
	@echo "🐳 Membangun image untuk Account Service..."
	podman build -t bank-account-service:latest -f services/account-service/Containerfile .
	@echo "🐳 Membangun image untuk Third-Party Adapter..."
	podman build -t bank-third-party-adapter:latest -f services/third-party-adapter/Containerfile .
	@echo "✅ Semua image berhasil dibangun!"

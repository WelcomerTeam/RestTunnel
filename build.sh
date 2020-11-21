echo "Build GO Executable"
go build -v -o resttunnel cmd/main.go

echo "Docker build and push"
docker build --tag 1345/resttunnel:latest .
docker push 1345/resttunnel:latest

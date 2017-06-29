# runc-compose
CLI to compose runc containers

## How to build from source
Make sure [go](https://golang.org/) 1.8 is installed.
Clone the repository and run the `./make.sh` script contained in its root directory to build and test the project:
```
git clone git@github.com:mgoltzsche/runc-compose.git &&
cd runc-compose &&
./make.sh
```
The binary is saved to `bin/runc-compose`. Include it into your `PATH` to be able to run the examples:
```
export PATH="$(pwd)/bin:$PATH"
```

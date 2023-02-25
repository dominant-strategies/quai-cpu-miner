# Quai CPU Miner

Quai CPU Miner is a cli app that utilizes CPU blake3 library to mine Quai Blockchain

## How to build the Quai CPU Miner
```
make quai-cpu-miner
```

## How to Run ?
Both MakeFile and command lines can be used to run the Quai CPU Miner.

For Example to run a Miner in `0,0` slice
### Running using the binary
```
./build/bin/quai-cpu-miner --region 0 --zone 0
```
It also supports `Aliases`, So running using Aliases will look like this, `--r` corresponds to `--region` and `--z` corresponds to `--zone`
```
./build/bin/quai-cpu-miner --r 0 --z 0
```

### Running using the Makefile
```
make run-mine region=0 zone=0
```

### Running using the Makefile in background
```
make run-mine-background region=0 zone=0
```
If running in Background the logs are saved in the `logs` folder

## How to stop the Miner
```
make stop
```

Additionally, all the flags can be seen in the help section using `--help` or `--h` command
```
./build/bin/quai-cpu-miner --help
```
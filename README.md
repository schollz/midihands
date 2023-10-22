# midihands

## ableton setup

you need a "virtual midi port". here's how to get one in ableton: https://help.ableton.com/hc/en-us/articles/209774225-Setting-up-a-virtual-MIDI-bus#Windows

## linux

Install `portmidi`:

```
sudo apt install libportmidi-dev
```

## windows build:

First [install msys2](https://www.msys2.org/). to C:\

Open msys2 and run

```bash
pacman -S mingw-w64-x86_64-gcc mingw-w64-x86_64-portmidi
```

Now before building *midihands* setup the environment:

```powershell
$env:CGO_ENABLED="1"
$env:CC="x86_64-w64-mingw32-gcc"
$env:CGO_CFLAGS = "-IC:\msys64\mingw64\include"
$env:CGO_LDFLAGS = "-LC:\msys64\mingw64\lib"
$env:PATH += ";C:\msys64\usr\bin;C:\msys64\mingw64\bin"
```

Now run 

```bash
go build
```
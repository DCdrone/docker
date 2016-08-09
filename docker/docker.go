package main

import (
	"fmt"
	"os"

	"github.com/docker/docker//api/client"
	"github.com/docker/docker//cli"
	"github.com/docker/docker//dockerversion"
	"github.com/docker/docker//pkg/term"
	"github.com/docker/docker//utils"

	"github.com/docker/docker//pkg/reexec"

	"github.com/Sirupsen/logrus"
	flag "github.com/docker/docker/pkg/mflag"
)

func main() {
	//启动前检查，防止出现已经启动的情况
	if reexec.Init() {
		return
	}

	// Set terminal emulation based on platform as required.
	stdin, stdout, stderr := term.StdStreams()

	logrus.SetOutput(stderr)

	//合并参数类型，所有的参数类型都是FlagSet类，该类定义如下：
	//type FlagSet struct {
	// Usage is the function called when an error occurs while parsing flags.
	// The field is a function (not a method) that may be changed to point to
	// a custom error handler.
	/*Usage      func()
		ShortUsage func()

		name             string
		parsed           bool
		actual           map[string]*Flag
		formal           map[string]*Flag
		args             []string // arguments after flags
		errorHandling    ErrorHandling
		output           io.Writer // nil means stderr; use Out() accessor
		nArgRequirements []nArgRequirement
	}*/
	//合并参数到flag.CommandLine中。CommandLine中对应的是Options。
	flag.Merge(flag.CommandLine, clientFlags.FlagSet, commonFlags.FlagSet)

	//打印docker的使用方式。这里其实只是设置Usage这个函数的实现，以便需要打印的时候打印。
	flag.Usage = func() {
		//第一行是正常的使用方式；第二行是daemon的使用方式；第三行固定输出
		fmt.Fprint(stdout, "Usage: docker [OPTIONS] COMMAND [arg...]\n"+daemonUsage+"       docker [ --help | -v | --version ]\n\n")
		fmt.Fprint(stdout, "A self-sufficient runtime for containers.\n\nOptions:\n")

		//输出Options的内容，打印CommandLine中的内容。这一行设置输出到标准输出。
		flag.CommandLine.SetOutput(stdout)
		//真实的打印内容
		flag.PrintDefaults()

		//开始打印可选择的命令Commands。
		help := "\nCommands:\n"

		//循环输出dockerCommands命令中的可选择命令，分别打印他们的名字和描述信息。
		//dockerCommands最终由cli.DockerCommands中提供。
		for _, cmd := range dockerCommands {
			help += fmt.Sprintf("    %-10.10s%s\n", cmd.Name, cmd.Description)
		}

		help += "\nRun 'docker COMMAND --help' for more information on a command."
		//真实的打印
		fmt.Fprintf(stdout, "%s\n", help)
	}

	//解析参数
	flag.Parse()

	//version单独处理，这里通过判断flVersion的返回结果是否为真进行处理。
	if *flVersion {
		showVersion()
		return
	}
	//help信息单独处理
	if *flHelp {
		// if global flag --help is present, regardless of what other options and commands there are,
		// just print the usage.
		flag.Usage()
		return
	}

	//创建client模式的docker，详细分析请见api/client/cli.go包中的NewDockerCli函数。
	clientCli := client.NewDockerCli(stdin, stdout, stderr, clientFlags)

	//合并client模式的docker和daemonCli模式的docker，实际中只会有一个在工作。
	//daemonCli对象的创建请见docker/daemon.go中的daemonCli cli.Handler = NewDaemonCli()
	//cli.New接受两个参数，分别是dockercli对象和daemoncli对象，两个对象结构不同。
	//但是返回的是一个cli对象:
	/*
			type Cli struct {
			    Stderr   io.Writer
			    handlers []Handler
			    Usage    func()
		            }
	*/
	//clientClie和daemoncli就放在句柄handlers数组中。
	c := cli.New(clientCli, daemonCli)
	//c.Run函数见cli中的cli.go的func (cli *Cli) Run(args ...string) error
	if err := c.Run(flag.Args()...); err != nil {
		if sterr, ok := err.(cli.StatusError); ok {
			if sterr.Status != "" {
				fmt.Fprintln(stderr, sterr.Status)
				os.Exit(1)
			}
			os.Exit(sterr.StatusCode)
		}
		fmt.Fprintln(stderr, err)
		os.Exit(1)
	}
}

func showVersion() {
	if utils.ExperimentalBuild() {
		fmt.Printf("Docker version %s, build %s, experimental\n", dockerversion.Version, dockerversion.GitCommit)
	} else {
		fmt.Printf("Docker version %s, build %s\n", dockerversion.Version, dockerversion.GitCommit)
	}
}

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/mackerelio/go-mackerel-plugin-helper"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type config struct {
	cluster       string
	service       string
	containerName string
	port          int

	networkMode string

	session    *session.Session
	ecsService *ecs.ECS
}

type target struct {
	id   string
	host string
	port int
}

var httpClient = &http.Client{}

func main() {
	c := config{}
	flag.StringVar(&c.cluster, "cluster", "default", "ECS cluster name")
	flag.StringVar(&c.service, "service", "", "ECS service name")
	flag.StringVar(&c.containerName, "containerName", "", "Container name")
	flag.IntVar(&c.port, "port", 2018, "Port")
	flag.StringVar(&c.networkMode, "networkMode", "bridge", "Network mode of the task")
	timeout := flag.Int("timeout", 30, "http timeout in seconds")
	flag.Parse()

	httpClient.Timeout = time.Second * time.Duration(*timeout)

	var err error
	c.session, err = session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	})
	if err != nil {
		panic(err)
	}
	if aws.StringValue(c.session.Config.Region) == "" {
		region, err := ec2metadata.New(c.session).Region()
		if err != nil {
			panic(err)
		}
		c.session.Config.Region = aws.String(region)
	}
	c.ecsService = ecs.New(c.session)

	list, err := c.getTargetList()
	if err != nil {
		panic(err)
	}
	if os.Getenv("MACKEREL_AGENT_PLUGIN_META") == "1" {
		printMetricsMeta(c.service, list)
	} else {
		printMetrics(c.service, list)
	}
}

func (c *config) listTasks() ([]*string, error) {
	ret := make([]*string, 0)
	var nextToken *string
	for {
		list, err := c.ecsService.ListTasks(&ecs.ListTasksInput{
			Cluster:     aws.String(c.cluster),
			ServiceName: aws.String(c.service),
			NextToken:   nextToken,
		})
		if err != nil {
			return nil, err
		}
		ret = append(ret, list.TaskArns...)
		nextToken = list.NextToken
		if nextToken == nil || aws.StringValue(nextToken) == "" {
			return ret, err
		}
	}
}

func (c *config) getTargetList() ([]target, error) {
	tasks, err := c.listTasks()
	if err != nil {
		return nil, err
	}

	ret := make([]target, 0, len(tasks))
	if len(tasks) == 0 {
		return ret, nil
	}

	desc, err := c.ecsService.DescribeTasks(&ecs.DescribeTasksInput{
		Cluster: aws.String(c.cluster),
		Tasks:   tasks,
	})
	if err != nil {
		return nil, err
	}

	switch c.networkMode {
	case "awsvpc":
		for _, task := range desc.Tasks {
			for _, container := range task.Containers {
				if aws.StringValue(container.Name) == c.containerName {
					if container.NetworkInterfaces != nil && len(container.NetworkInterfaces) > 0 {
						addr := container.NetworkInterfaces[0].PrivateIpv4Address
						ret = append(ret, target{
							id:   extractIDFromArn(task.TaskArn),
							host: aws.StringValue(addr),
							port: c.port,
						})
					}
				}
			}
		}
	case "bridge":
		arnToAddr, err := c.createArnToAddrMap(desc.Tasks)
		if err != nil {
			return nil, err
		}
		for _, task := range desc.Tasks {
			for _, container := range task.Containers {
				if aws.StringValue(container.Name) == c.containerName {
					if container.NetworkBindings != nil {
						for _, b := range container.NetworkBindings {
							if aws.Int64Value(b.ContainerPort) == int64(c.port) {
								if addr, ok := arnToAddr[aws.StringValue(task.ContainerInstanceArn)]; ok {
									ret = append(ret, target{
										id:   extractIDFromArn(task.TaskArn),
										host: addr,
										port: int(aws.Int64Value(b.HostPort)),
									})
								}
							}
						}
					}
				}
			}
		}
	case "host":
		arnToAddr, err := c.createArnToAddrMap(desc.Tasks)
		if err != nil {
			return nil, err
		}
		for _, task := range desc.Tasks {
			if addr, ok := arnToAddr[aws.StringValue(task.ContainerInstanceArn)]; ok {
				ret = append(ret, target{
					id:   extractIDFromArn(task.TaskArn),
					host: addr,
					port: c.port,
				})
			}
		}
	}
	return ret, nil
}

func (c *config) createArnToAddrMap(tasks []*ecs.Task) (map[string]string, error) {
	arns := make([]string, 0, len(tasks))
	for _, t := range tasks {
		arns = append(arns, aws.StringValue(t.ContainerInstanceArn))
	}
	arns = distinct(arns)

	desc, err := c.ecsService.DescribeContainerInstances(&ecs.DescribeContainerInstancesInput{
		Cluster:            aws.String(c.cluster),
		ContainerInstances: aws.StringSlice(arns),
	})
	if err != nil {
		return nil, err
	}
	idToArn := make(map[string]string)
	for _, d := range desc.ContainerInstances {
		idToArn[aws.StringValue(d.Ec2InstanceId)] = aws.StringValue(d.ContainerInstanceArn)
	}

	ec2Service := ec2.New(c.session)
	var nextToken *string
	ids := aws.StringSlice(keys(idToArn))
	arnToHost := make(map[string]string)
	for {
		desc, err := ec2Service.DescribeInstances(&ec2.DescribeInstancesInput{
			InstanceIds: ids,
			NextToken:   nextToken,
		})
		if err != nil {
			return nil, err
		}
		for _, r := range desc.Reservations {
			for _, i := range r.Instances {
				if i.NetworkInterfaces != nil && len(i.NetworkInterfaces) > 0 {
					arn := idToArn[aws.StringValue(i.InstanceId)]
					arnToHost[arn] = aws.StringValue(i.NetworkInterfaces[0].PrivateIpAddress)
				}
			}
		}
		nextToken = desc.NextToken
		if nextToken == nil || aws.StringValue(nextToken) == "" {
			return arnToHost, nil
		}
	}
}

func distinct(src []string) []string {
	m := make(map[string]string)
	for _, v := range src {
		m[v] = ""
	}
	return keys(m)
}

func keys(src map[string]string) []string {
	ret := make([]string, 0, len(src))
	for k := range src {
		ret = append(ret, k)
	}
	return ret
}

func extractIDFromArn(arn *string) string {
	parts := strings.Split(aws.StringValue(arn), "/")
	return parts[len(parts)-1]
}

func printMetrics(service string, list []target) {
	wg := sync.WaitGroup{}
	wg.Add(len(list))

	for _, t := range list {
		go func(t target) {
			defer wg.Done()

			url := fmt.Sprintf("http://%s:%d/", t.host, t.port)
			res, err := httpClient.Get(url)
			if err != nil {
				fmt.Fprintf(os.Stderr, "fail to fetch metrics from %s : %v", url, err)
				return
			}
			defer res.Body.Close()
			if res.StatusCode != 200 || res.Header.Get("Content-Type") != "text/plain" {
				fmt.Fprintf(os.Stderr, "fail to fetch metrics from %s : %v", url, err)
				return
			}

			scanner := bufio.NewScanner(res.Body)
			for scanner.Scan() {
				fmt.Println(service + "." + t.id + "." + scanner.Text())
			}
			if err := scanner.Err(); err != nil {
				fmt.Fprintf(os.Stderr, "body read error: %v", err)
			}
		}(t)
	}
	wg.Wait()
}

type Meta struct {
	Graphs map[string]mackerelplugin.Graphs `json:"graphs"`
}

func getMetricsMeta(t *target) (*Meta, error) {
	url := fmt.Sprintf("http://%s:%d/", t.host, t.port)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fail to fetch metrics meta from %s : %v", url, err)
		return nil, err
	}

	req.Header.Set("X-MACKEREL-AGENT-PLUGIN-META", "1")
	res, err := httpClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fail to fetch metrics meta from %s : %v", url, err)
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "text/json" || res.Header.Get("X-MACKEREL-AGENT-PLUGIN-META") != "1" {
		fmt.Fprintf(os.Stderr, "fail to fetch metrics meta from %s", url)
		return nil, errors.New("invalid responce")
	}

	b, err := ioutil.ReadAll(res.Body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fail to fetch metrics meta from %s : %v", url, err)
		return nil, err
	}
	meta := &Meta{}
	err = json.Unmarshal(b, meta)
	if err != nil {
		fmt.Fprintf(os.Stderr, "recieved invalid metrics meta from %s : %v", url, err)
		return nil, err
	}
	return meta, nil

}

func printMetricsMeta(service string, list []target) {
	fmt.Println("# mackerel-agent-plugin")

	for _, t := range list {
		srcMeta, err := getMetricsMeta(&t)
		if err != nil {
			continue
		}

		dstMeta := Meta{Graphs: make(map[string]mackerelplugin.Graphs)}
		for k, g := range srcMeta.Graphs {
			k = service + ".#." + k
			g.Label = fmt.Sprintf("[%s] %s", service, g.Label)
			dstMeta.Graphs[k] = g
		}
		b, err := json.Marshal(&dstMeta)
		if err != nil {
			panic(err)
		}

		fmt.Println(string(b))
		return
	}
}

package logspout

import (
    "github.com/gliderlabs/logspout/router"
    "github.com/awslabs/aws-sdk-go/aws"
    "github.com/awslabs/aws-sdk-go/service/cloudwatchlogs"
    "github.com/fsouza/go-dockerclient"
    "fmt"
    "strings"
    "log"
    "errors"
    "encoding/json"
)

func init() {
    router.AdapterFactories.Register(NewCloudWatchLogsAdapter, "cloudwatchlogs")
}

type CloudWatchLogsAdapter struct {
    cwl *cloudwatchlogs.CloudWatchLogs
    route *router.Route
    streamPrefix string
    defaultLogGroup string
    logGroupEnvPrefix string
    containerLogGroups map[string]string
    containerStreams map[string]*containerStream
}

func NewCloudWatchLogsAdapter(route *router.Route) (router.LogAdapter, error) {
    cwl := cloudwatchlogs.New(&aws.Config{
        Region: route.Address,
    })
    for k := range route.Options {
        if k != "stream-prefix" && k != "default-log-group" && k != "log-group-env-prefix" && k != "container-log-groups" {
            return nil, errors.New(fmt.Sprintf("unknown option %s", k))
        }
    }
    var containerLogGroups map[string]string
    if val, ok := route.Options["container-log-groups"]; ok {
        if err := json.Unmarshal([]byte(val), &containerLogGroups); err != nil {
            return nil, errors.New(fmt.Sprintf("error parsing container-log-groups %s", err))
        }
    }
    return &CloudWatchLogsAdapter{
        cwl: cwl,
        route: route,
        streamPrefix: route.Options["stream-prefix"],
        defaultLogGroup: route.Options["default-log-group"],
        logGroupEnvPrefix: route.Options["log-group-env-prefix"],
        containerLogGroups: containerLogGroups,
        containerStreams: make(map[string]*containerStream),
    }, nil
}

func (self *CloudWatchLogsAdapter) Stream(logstream chan *router.Message) {
    for m := range logstream {
        name := strings.Join([]string{ self.streamPrefix, m.Container.Name[1:], m.Source }, "-")

        var stream = self.containerStreams[name]

        if stream == nil {
            stream = self.createContainerStream(name, m.Container, m.Source)
            self.containerStreams[name] = stream
        }

        if stream.channel != nil {
            stream.channel <- m
        }
    }
}

func (self *CloudWatchLogsAdapter) createContainerStream (name string, container *docker.Container, source string) *containerStream {
    logGroupName := self.getLogGroupName(container, source)
    var channel chan *router.Message
    if logGroupName != "" {
        channel = make(chan *router.Message)
    }
    stream := containerStream{
        logGroupName: logGroupName,
        channel: channel,
        adapter: self,
        seenLogStream: false,
        streamName: name,
    }
    if stream.channel != nil {
        go stream.handleMessages()
    }
    return &stream
}

func (self *CloudWatchLogsAdapter) getLogGroupName (container *docker.Container, source string) string {
    if logGroup, explicitLogGroup := self.containerLogGroups[container.Name[1:]]; explicitLogGroup {
        return logGroup
    } else if self.logGroupEnvPrefix != "" {
        prefix := strings.Join([]string{ self.logGroupEnvPrefix, "_", strings.ToUpper(source), "=" }, "")
        for i := 0; i < len(container.Config.Env); i++ {
            if strings.HasPrefix(container.Config.Env[i], prefix) {
               return container.Config.Env[i][len(prefix):]
            }
        }
    }
    return self.defaultLogGroup
}

type containerStream struct {
    logGroupName string
    streamName string
    channel chan *router.Message
    adapter *CloudWatchLogsAdapter
    seenLogStream bool
    nextSequenceToken *string
}

func (self *containerStream) getNextSequenceToken (m *router.Message) (*string, error) {
    if self.seenLogStream {
        return self.nextSequenceToken, nil
    }
    logStreamsDescription, descErr := self.adapter.cwl.DescribeLogStreams(&cloudwatchlogs.DescribeLogStreamsInput{
        LogGroupName: &self.logGroupName,
        LogStreamNamePrefix: &self.streamName,
    })
    if descErr != nil {
        return nil, descErr
    }
    if len(logStreamsDescription.LogStreams) == 0 {
        _, createErr := self.adapter.cwl.CreateLogStream(&cloudwatchlogs.CreateLogStreamInput{
            LogGroupName: &self.logGroupName,
            LogStreamName: &self.streamName,
        })
        if createErr != nil {
            return nil, createErr
        }
        self.seenLogStream = true
        return nil, nil
    } else {
        if *logStreamsDescription.LogStreams[0].LogStreamName != self.streamName {
            return nil, errors.New(fmt.Sprintf("unexpected log stream %s matching %s (%d streams returned)",
                *logStreamsDescription.LogStreams[0].LogStreamName,
                self.streamName,
                len(logStreamsDescription.LogStreams),
            ))
        }
        self.seenLogStream = true
        self.nextSequenceToken = logStreamsDescription.LogStreams[0].UploadSequenceToken
        return self.nextSequenceToken, nil
    }
}

func (self *containerStream) handleMessages () {
    for {
        m := <-self.channel
        sequenceToken, err := self.getNextSequenceToken(m)
        if err != nil {
            // TODO do something more sophisticated than dropping the message on the floor
            // TODO backoff on failure
            log.Print("error getting log stream: ", err)
            continue
        }
        putResponse, putErr := self.adapter.cwl.PutLogEvents(&cloudwatchlogs.PutLogEventsInput{
            LogEvents: []*cloudwatchlogs.InputLogEvent{
                &cloudwatchlogs.InputLogEvent{
                    Message: aws.String(m.Data),
                    Timestamp: aws.Long(m.Time.UnixNano() / (1000 * 1000)),
                },
            },
            LogGroupName: &self.logGroupName,
            LogStreamName: &self.streamName,
            SequenceToken: sequenceToken,
        })
        if putErr != nil {
            // TODO do something more sophisticated than dropping message on the floor
            // TODO backoff on failure
            log.Print("error putting log events: ", putErr)
            continue
        }
        self.nextSequenceToken = putResponse.NextSequenceToken
    }
}


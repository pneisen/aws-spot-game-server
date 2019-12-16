package main

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/route53"
)

// GameServerUserData is the data retrieved from the AWS UserData spec'd in the launch.
type GameServerUserData struct {
	HostedZone                      string
	DNSName                         string
	VolumeID                        string
	RunPath                         string
	StopPath                        string
	IdlePath                        string
	IdleInterval                    int
	IdleConsecutiveTimesForShutdown int
}

func getInstanceID() (string, error) {
	resp, err := http.Get("http://169.254.169.254/latest/meta-data/instance-id")
	if err != nil {
		return "", err
	}

	id, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return "", err
	}

	return string(id), nil
}

func getPublicIP() (string, error) {
	resp, err := http.Get("http://169.254.169.254/latest/meta-data/public-ipv4")
	if err != nil {
		return "", err
	}

	ip, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return "", err
	}

	return string(ip), nil
}

func getUserData() (*GameServerUserData, error) {
	resp, err := http.Get("http://169.254.169.254/latest/user-data")
	if err != nil {
		return nil, err
	}

	data, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}

	sliced := strings.Split(strings.Trim(string(data), "\n"), "|")

	if len(sliced) != 8 {
		return nil, fmt.Errorf("user data was malformed or not complete")
	}

	interval, err := strconv.Atoi(sliced[6])
	if err != nil {
		return nil, fmt.Errorf("idle interval was malformed")
	}

	times, err := strconv.Atoi(sliced[7])
	if err != nil {
		return nil, fmt.Errorf("idle consecutive times for shutdown was malformed")
	}

	return &GameServerUserData{
		HostedZone:                      sliced[0],
		DNSName:                         sliced[1],
		VolumeID:                        sliced[2],
		RunPath:                         sliced[3],
		StopPath:                        sliced[4],
		IdlePath:                        sliced[5],
		IdleInterval:                    interval,
		IdleConsecutiveTimesForShutdown: times,
	}, nil
}

func checkTermination(userData *GameServerUserData) {
	_, err := os.Stat(userData.StopPath)
	if err != nil {
		// if the stop path doesn't exit, no reason to run the goroutine
		return
	}

	// Spin this off in a goroutine
	go func() {
		resp, err := http.Get("http://169.254.169.254/latest/meta-data/spot/termination-time")
		if err != nil {
			fmt.Printf("Error getting termination time: %s\n", err.Error())
		} else {
			if resp.StatusCode != 404 {
				fmt.Printf("We got notification of termination. Calling stop and exiting.\n")
				cmd := exec.Command(userData.StopPath)
				err := cmd.Run()
				if err != nil {
					fmt.Printf("Error calling stop: %s\n", err.Error())
				}
				return
			}
			resp.Body.Close()
		}

		// Sleep 5 seconds and check again.
		time.Sleep(5 * time.Second)
	}()
}

func checkIdle(userData *GameServerUserData, instanceID string, sess *session.Session) {
	_, err := os.Stat(userData.IdlePath)
	if err != nil {
		// If the idle path doesn't exit, no reason to run the goroutine
		return
	}

	_, err = os.Stat(userData.StopPath)
	if err != nil {
		// if the stop path doesn't exit, no reason to run the goroutine
		return
	}

	// Spin this off in a goroutine
	go func() {
		count := 0
		for {
			// Call the idle script. If the exit status is 0, the game server is idle and should count this iteration.
			// Otherwise, the server is not idle and we reset the count.
			cmd := exec.Command(userData.IdlePath)
			err := cmd.Run()
			if err != nil {
				// exit status != 0, game server is not idle, reset the count.
				fmt.Println("Game server active, resetting count.")
				count = 0
			} else {
				// exit status == 0, game server is idle, increment the count and check the threshold.
				fmt.Println("Game server idle, incrementing count.")
				count = count + 1
				if count >= userData.IdleConsecutiveTimesForShutdown {
					// We have been idle too long. Shutdown.
					fmt.Printf("Game server has been idle too long. Calling stop and exiting.\n")
					cmd := exec.Command(userData.StopPath)
					err := cmd.Run()
					if err != nil {
						fmt.Printf("Error calling stop: %s\n", err.Error())
					}

					// Terminate the instance as well.
					service := ec2.New(sess)

					input := &ec2.TerminateInstancesInput{
						DryRun:      aws.Bool(false),
						InstanceIds: []*string{aws.String(instanceID)},
					}

					_, err = service.TerminateInstances(input)
					if err != nil {
						fmt.Printf("Terminating instances failed: %s\n", err.Error())
					}
					return
				}
			}
			time.Sleep(time.Duration(userData.IdleInterval) * time.Second)
		}
	}()
}

func setDNS(userData *GameServerUserData, sess *session.Session) error {
	fmt.Println("Getting public ip.")
	publicIP, err := getPublicIP()
	if err != nil {
		return fmt.Errorf("error getting public IP: %s", err.Error())
	}

	service := route53.New(sess)
	var ttl int64 = 300
	input := &route53.ChangeResourceRecordSetsInput{
		ChangeBatch: &route53.ChangeBatch{
			Changes: []*route53.Change{
				{
					Action: aws.String("UPSERT"),
					ResourceRecordSet: &route53.ResourceRecordSet{
						Name: aws.String(userData.DNSName),
						Type: aws.String("A"),
						TTL:  &ttl,
						ResourceRecords: []*route53.ResourceRecord{
							{
								Value: aws.String(publicIP),
							},
						},
					},
				},
			},
			Comment: aws.String("Game Server"),
		},
		HostedZoneId: aws.String(userData.HostedZone),
	}

	_, err = service.ChangeResourceRecordSets(input)
	if err != nil {
		return fmt.Errorf("error setting DNS: %s", err.Error())
	}

	fmt.Println("DNS set.")
	return nil
}

func mountVolume(userData *GameServerUserData, instanceID string, sess *session.Session) error {
	service := ec2.New(sess)

	fmt.Println("Attaching volume.")

	// Try for up to 2 minutes
	attached := false
	for i := 0; i < 24; i++ {
		input := &ec2.AttachVolumeInput{
			Device:     aws.String("/dev/sdf"),
			InstanceId: aws.String(instanceID),
			VolumeId:   aws.String(userData.VolumeID),
		}

		_, err := service.AttachVolume(input)

		if err != nil {
			fmt.Printf("Error attaching volume: %s\n", err.Error())
		} else {
			attached = true
			break
		}
		time.Sleep(5 * time.Second)
	}

	if !attached {
		return fmt.Errorf("errors attaching volume - giving up")
	}

	fmt.Println("Volume attached. Looking for device file")
	found := false
	deviceFile := ""
	// Try for up to 2 minutes
	for i := 0; i < 24; i++ {
		_, err := os.Stat("/dev/xvdf")
		_, err2 := os.Stat("/dev/nvme1n1")
		if err == nil || err2 == nil {
			found = true
			if err != nil {
				deviceFile = "/dev/nvme1n1"
			} else {
				deviceFile = "/dev/xvdf"
			}
			break
		}
		time.Sleep(5 * time.Second)
	}

	if !found {
		return fmt.Errorf("Device file not found")
	}

	fmt.Println("Creating mount point.")
	oldUMask := syscall.Umask(0)
	err := os.Mkdir("/mnt/game", 0777)
	if err != nil {
		return fmt.Errorf("error creating mount point: %s", err.Error())
	}
	syscall.Umask(oldUMask)

	fmt.Println("Mounting volume.")
	err = syscall.Mount(deviceFile, "/mnt/game", "ext4", 0, "")
	if err != nil {
		return fmt.Errorf("error mounting volume: %s", err.Error())
	}

	fmt.Println("Volume mounted.")

	return nil
}

func startGame(userData *GameServerUserData) error {
	_, err := os.Stat(userData.RunPath)
	if err != nil {
		return fmt.Errorf("error starting game server: %s", err.Error())
	}

	fmt.Println("Starting game server.")
	//	screen := "/usr/bin/screen -dm -S gameserver /bin/bash " + userData.RunPath
	//	cmd := exec.Command("/bin/su", "ubuntu", "-c", screen)
	cmd := exec.Command("/bin/su", "ubuntu", "-c", userData.RunPath)
	cmd.Stdout = os.Stdout

	err = cmd.Run()
	if err != nil {
		return fmt.Errorf("game server returned error: %s", err.Error())
	}

	fmt.Println("Game server done.")
	return nil
}

func main() {
	fmt.Println("Getting user data.")
	userData, err := getUserData()
	if err != nil {
		fmt.Printf("Error getting user data: %s\n", err.Error())
		os.Exit(1)
	}

	fmt.Println("Getting instance id.")
	instanceID, err := getInstanceID()
	if err != nil {
		fmt.Printf("Error getting instance ID: %s\n", err.Error())
		os.Exit(1)
	}

	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))

	err = setDNS(userData, sess)
	if err != nil {
		fmt.Printf("Error setting DNS: %s\n", err.Error())
		os.Exit(1)
	}

	err = mountVolume(userData, instanceID, sess)
	if err != nil {
		fmt.Printf("Error mounting volume: %s\n", err.Error())
		os.Exit(1)
	}

	checkTermination(userData)

	checkIdle(userData, instanceID, sess)

	err = startGame(userData)
	if err != nil {
		fmt.Printf("Error starting game: %s\n", err.Error())
	}
}

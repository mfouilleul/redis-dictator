package main

import (
	log "github.com/Sirupsen/logrus"
	"time"
	"github.com/samuel/go-zookeeper/zk"
	"strconv"
	"strings"
	"errors"
	"encoding/json"
)

type Elector struct {
	ZKHosts []string
	ZKConnection *zk.Conn
	ZKPathElection string
	ZKPathService string
	ZKPathMaster string
	MyToken string
	ZKEvent <-chan zk.Event
	Redis *Redis
}

func(ze *Elector) Initialize(ZKHosts []string, serviceName string, Redis *Redis) (error) {
	ze.ZKPathElection = "/elections/redis/" + serviceName
	ze.ZKPathService = "/services/redis/" + serviceName
	ze.ZKPathMaster = "/services/redis/" + serviceName + "/master"
	ze.ZKConnection = nil
	ze.ZKHosts = ZKHosts
	ze.Redis = Redis
	ze.MyToken = ""
	return nil
}


type ZKDebugLogger struct {}

func(ZKDebugLogger) Printf(format string, a ...interface{}) {
	log.Debug(format, a)
}

func(ze *Elector) Run(){
	for{
		//Test Connection to ZooKeeper
		state, err := ze.ZKConnect() //internally the connection is maintained
		if err != nil {
			log.Warn("Connection to Zookeeper Fail")
		}
		if state == zk.StateHasSession {
			masterExists, _, events, err := ze.ZKConnection.ExistsW(ze.ZKPathMaster)
			if err != nil {
				log.Warn("Unable to watch master key.")
			}else{
				if masterExists{
					if ze.Redis.Role == "UNKNOWN" {
			        	master, err := ze.GetMasterNode()
			        	if err != nil {
			        		log.Warn("Unable to get the master infos...")
			        	}else{
							err = ze.Redis.SetRole("SLAVE", master)
				        	if err != nil {
				        		log.Warn("Unable to change node role.")
				        	}else{
		        				log.Info("I'm slave")
		        			}
		        		}
					}	
				}else{
					log.Info("There is no master...")
					err := ze.NewElection()
					if err != nil {
						log.Warn(err)
						// Reset role to force retake position
						ze.Redis.Role = "UNKNOWN"
					}
				}
				// We can now watch the master key
				select{
					case ev := <-events:
						if ev.Err != nil{
							log.Warn("Error with Zookeeper: ", ev.Err)
						}
					case ev := <-ze.ZKEvent:
						if ev.Err != nil{
							log.Warn("Error with Zookeeper: ", ev.Err)
						}
					break
				}
			}
		}
		time.Sleep(time.Millisecond * 200)
	}
}

func(ze *Elector) ElectionGetMembers()([]int, error){
	members, _, err := ze.ZKConnection.Children(ze.ZKPathElection)
	if err != nil {
		return nil, err
	}
	var members_int []int
	for _, m := range members {
		m_int, _ := strconv.Atoi(m)
    	members_int = append(members_int, m_int)
    }
	return members_int, nil
}


func(ze *Elector) GetMasterNode()(*Redis, error){
	master_json, _, err := ze.ZKConnection.Get(ze.ZKPathMaster)
	if err != nil {
		return nil, err
	}
	var master_map map[string]string
	err = json.Unmarshal(master_json, &master_map)
	if err != nil {
        return nil, err
    }
    var master Redis
    master.Name = master_map["name"]
    master.Host = master_map["host"]
    _port, _ := strconv.Atoi(master_map["port"])
    master.Port = _port
	master.Role = "MASTER"
	return &master, nil
}


func(ze *Elector) ElectionTakePosition()(string, error){
	// Create Elections Path if doesn't not exists
	err := ze.ZKCreatePath(ze.ZKPathElection)
	if err != nil { // Maybe another node has created the path in the same time, test it before raise error
		exists, _, _ := ze.ZKConnection.Exists(ze.ZKPathElection)
		if !exists {
			return "", err
		}
	}
	path, err := ze.ZKCreateNode(ze.ZKPathElection + "/", "", zk.FlagEphemeral|zk.FlagSequence)
	if err != nil {
		return "", err
	}
	nodes := strings.Split(path, "/")
	token := nodes[len(nodes)-1]
	// Return my token
	// a zk sequence node (string)
	return token, nil
}

func(ze *Elector) ElectionCleanMyToken()(error){
	if ze.MyToken != "" {
		exists, _, _ := ze.ZKConnection.Exists(ze.ZKPathElection + "/" + ze.MyToken)
		if exists {
			err := ze.ZKConnection.Delete(ze.ZKPathElection + "/" + ze.MyToken, -1)
			return err
		}
	}
	return nil
}

func(ze *Elector) NewElection()(error){
	log.Info("Starting a new election.")
	// Clean my token - Should not be necessary
	// Usefull if someone manually delete the master node during while dictator is running
	err := ze.ElectionCleanMyToken()
	if err != nil {
		log.Warn("Error during token cleanning.")
		return errors.New("Election Failed!")
	}
	ze.MyToken, err = ze.ElectionTakePosition()
	if err != nil {
		log.Warn("Unable to take position in election...")
		return errors.New("Election Failed!")
	}
	members, err := ze.ElectionGetMembers()
	if err != nil {
		log.Warn("Unable to get election members...")
		return errors.New("Election Failed!")
	}
	if members != nil {
		master_token := members[0]
        for _, member_token := range members {
        	if member_token < master_token {
            	master_token = member_token
            }
        }
        my_token, _ := strconv.Atoi(ze.MyToken)
        if my_token  == master_token {
        	log.Info("I'm Master!")
        	err := ze.PersistMasterInfo()
        	if err != nil {
        		log.Warn("Unable to persist master infos...")
        		return errors.New("Election Failed!")
        	}
        	err = ze.Redis.SetRole("MASTER", nil)
        	if err != nil {
        		log.Warn("Unable to change node role to MASTER...")
				err := ze.ZKConnection.Delete(ze.ZKPathMaster, -1)
				if err == nil {
					log.Warn("Clean the Zookeeper node master to be consistent.")
				}
        		return errors.New("Election Failed!")
        	}
        }else{
        	master, err := ze.GetMasterNode()
        	if err != nil {
				log.Warn("Unable to get master infos...")
				return errors.New("Election Failed!")
        	}
        	err = ze.Redis.SetRole("SLAVE", master)
        	if err != nil {
        		log.Warn("Unable to change node role to SLAVE...")
        		return errors.New("Election Failed!")
        	}
        }
	}else{
		log.Info("There is no member in election...")
		return errors.New("Election Failed!")
	}
	return nil
}

func(ze *Elector) PersistMasterInfo()(error){
	jdata := "{\"host\": \"" + ze.Redis.Host + "\", \"port\": \"" + strconv.Itoa(ze.Redis.Port) + "\", \"name\": \"" + ze.Redis.Name + "\"}"
	err := ze.ZKCreatePath(ze.ZKPathService)
	if err != nil {
		return err
	}
	_, err = ze.ZKCreateNode(ze.ZKPathService + "/master", jdata, 1)
	if err != nil {
		return err
	}
	return nil
}



func(ze *Elector) ZKConnect() (zk.State, error) {
	if ze.ZKConnection != nil {
		state := ze.ZKConnection.State()
		switch state {
			case zk.StateUnknown,zk.StateConnectedReadOnly,zk.StateExpired,zk.StateAuthFailed,zk.StateConnecting: {
				//Disconnect, and let Reconnection happen
				log.Warn("Zookeeper connection is in BAD State [",state,"] reconnect")
				ze.ZKConnection.Close()
			}
			case zk.StateConnected, zk.StateHasSession: {
				log.Debug("Zookeeper connection connected(",state,"), nothing to do.")

				return state, nil
			}
			case zk.StateDisconnected: {
				log.Warn("Reporter connection is Disconnected -> Reconnection")
			}
		}
	}
	conn, ev, err := zk.Connect(ze.ZKHosts, 10 * time.Second)
	if err != nil {
		ze.ZKConnection = nil
		log.Warn("Unable to connect to ZooKeeper (",err,")")
		return zk.StateDisconnected, err
	}

	ze.ZKConnection = conn

	var zkLogger ZKDebugLogger
	ze.ZKConnection.SetLogger(zkLogger)

	state := ze.ZKConnection.State()

	ze.ZKEvent = ev
	return state, nil
}

func(ze *Elector) ZKCreateNode(path string, data string, flag int32)(string, error){
	p, err := ze.ZKConnection.Create(path, []byte(data), flag, zk.WorldACL(zk.PermAll))
	return p, err
}

func(ze *Elector) ZKCreatePath(path string) error {
	paths := strings.Split(path, "/")
	full := ""
	for i, node := range paths {
		if i > 0 {
			full +=  "/"
		}
		full += node
		exists, _, err := ze.ZKConnection.Exists(full)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		_, err = ze.ZKConnection.Create(full, []byte(""), int32(0), zk.WorldACL(zk.PermAll))
		if err != nil {
			return err
		}
	}
	return nil
}
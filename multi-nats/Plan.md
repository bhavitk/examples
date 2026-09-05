There are four different nats

Remote nats and three other nats. These three other nats known as device nats

all device nats connects to remote nats via leaf node. Each device nats publish message on subject: device.a(1|2|3).outgoing.data

Example message for each device nats subject:
device.a1.outgoing.data payload {"name": "a1", value: "{Random value}"}
device.a2.outgoing.data payload {"name": "a2", value: "{Random value}"}
device.a3.outgoing.data payload {"name": "a3", value: "{Random value}"}


Remote nats recieve messages from all of these device nats.

Remote nats can send message to any device nats with subject: device.a1.incoming.data

Remote nats sending data to device should be received by device with its own device id. here device id called a1. a1 should not receive data other a1.

Implement all of these under docker

1. Create remote nats
2. Create three device nats
3. Create remote nats subscriber and publisher which receives and sends data
4. Create publisher and subscriber for each device nats



claude --resume 39dc4e79-5532-4742-97cf-c19fc7370107
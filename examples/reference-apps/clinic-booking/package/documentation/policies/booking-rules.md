# Booking and cancellation rules

These rules apply to every clinic site of the group. They are synthetic text written for the AlixAI reference application; no real clinic is described.

## When an appointment can be booked

- An appointment can be booked from 60 minutes to 90 days ahead. A slot that starts sooner than 60 minutes from now, or later than 90 days from now, cannot be booked.
- A slot is booked by exactly one patient. If two people ask for the same slot, the first confirmed booking wins and the second is told the slot is no longer free.
- A patient cannot hold two appointments that overlap in time at the same clinic site.
- A receptionist can book for any patient of the site. A patient can book only for themselves. A doctor cannot book appointments.

## Confirming a booking

- Before anything is booked the clinic shows the exact patient, doctor, date and time and clinic site, and waits for a clear yes. Nothing is booked until the booking is confirmed.
- A booking waits 10 minutes for the confirmation. After that it is dropped and the slot is offered again.
- When the booking is made the patient receives a confirmation message at the phone number or email address on file, and an appointment reference such as APT-7K3Q9X.

## Cancelling an appointment

- An appointment can be cancelled up to the moment it starts. An appointment that has started or passed cannot be cancelled.
- A receptionist can cancel any appointment of the site, a doctor only their own appointments, a patient only their own.
- The clinic shows which appointment will be cancelled and waits for a clear yes before cancelling.
- When an appointment is cancelled its slot becomes free again and the patient receives a cancellation message.
- Possible reasons for a cancellation are: the patient asked, the doctor is unavailable, a scheduling error, or another reason.

## Changing an appointment

The clinic does not move appointments. To change the time, cancel the appointment and book a new slot.
